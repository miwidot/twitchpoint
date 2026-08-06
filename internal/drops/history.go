package drops

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Erfolgsliste ("Erhalten")
//
// Eine schmale, fortgeschriebene Liste aller erhaltenen Drops — eine Zeile je
// Drop, für immer. Zweck ist rein informativ: nachschauen können, was man wann
// bekommen hat, auch lange nachdem die Kampagne bei Twitch verschwunden ist.
//
// Bewusst getrennt vom Claim-Gedächtnis in der config (siehe localclaims.go):
//   - Das Gedächtnis ist Arbeitsmaterial, muss schnell nachschlagbar sein und
//     wird nach 90 Tagen aufgeräumt. Es enthält nur Kennung und Datum.
//   - Diese Liste ist zum Anschauen und darf bleiben. Sie enthält die Namen,
//     die es im Gedächtnis nicht gibt — und ohne sie hätte man nur
//     nichtssagende Kennungen wie "40edc785-8645-11f1-…".
//
// Warum überhaupt eine eigene Datei, wenn die Claims auch im Debug-Log stehen:
// Die Tageslogs sind ~5 MB pro Tag, zu 99,9 % Rauschen, und werden nach 14
// Tagen gelöscht. Die Erfolgsliste wächst um wenige Zeilen am Tag.
//
// Format: JSON Lines (eine JSON-Zeile je Eintrag). Fortschreiben ist damit ein
// einfaches Anhängen, ein abgebrochener Schreibvorgang beschädigt höchstens die
// letzte Zeile, und der Leser überspringt kaputte Zeilen einfach.

// claimHistoryFile ist der Dateiname neben der config.json. Absichtlich NICHT
// im logs-Verzeichnis: dort räumt die Aufbewahrungsgrenze auf.
const claimHistoryFile = "claimed-history.jsonl"

// claimHistoryMax begrenzt, wie viele Einträge die Weboberfläche bekommt.
// Die Datei selbst wird nie gekürzt.
const claimHistoryMax = 2000

// ClaimHistoryEntry ist ein erhaltener Drop.
type ClaimHistoryEntry struct {
	At       time.Time `json:"at"`       // wann wir den Claim zuerst gesehen haben
	Game     string    `json:"game"`     // z.B. "Marbles on Stream"
	Campaign string    `json:"campaign"` // z.B. "MarbleFest - July'26-Day2"
	Reward   string    `json:"reward"`   // z.B. "30 Tournament Coins"
	DropID   string    `json:"drop_id"`  // zur Nachverfolgung im Log
	Source   string    `json:"source"`   // "auto" = selbst geclaimt, "extern" = bei Twitch geclaimt
}

// claimHistory schreibt und liest die Erfolgsliste. Der Mutex schützt gegen
// gleichzeitiges Anhängen (Drops-Zyklus) und Lesen (Weboberfläche).
type claimHistory struct {
	mu   sync.Mutex
	path string
}

// newClaimHistory legt die Liste neben die angegebene config-Datei.
func newClaimHistory(configPath string) *claimHistory {
	dir := filepath.Dir(configPath)
	if dir == "" || dir == "." {
		dir = "config"
	}
	return &claimHistory{path: filepath.Join(dir, claimHistoryFile)}
}

// ensureWritable prüft beim Start, ob die Liste angelegt bzw. fortgeschrieben
// werden kann, und legt sie dabei gleich an.
//
// Existiert: weil ein Rechteproblem sonst erst beim nächsten Claim auffällt —
// und das kann Stunden später sein. Genau so passiert am 28.07.2026: Die Datei
// war von außen mit falschem Eigentümer angelegt worden, der Fehler
// ("permission denied") stand erst vier Stunden später im Log, und drei
// erhaltene Drops fehlten in der Liste.
//
// Fallstrick dahinter: Der Container läuft als root, aber der Stack setzt
// `cap_drop: ALL`. Damit fehlt root auch CAP_DAC_OVERRIDE — er darf Dateirechte
// also NICHT mehr ignorieren und wird wie ein normaler Benutzer behandelt. Eine
// Datei, die jemand anderem gehört und nur 644 hat, ist damit unbeschreibbar,
// obwohl "root" draufsteht.
func (h *claimHistory) ensureWritable() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(h.path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Append hängt Einträge an. Fehler sind nicht fatal — eine fehlende
// Erfolgsliste darf das Farmen nicht stören, deshalb gibt der Aufrufer den
// Fehler höchstens ins Log.
func (h *claimHistory) Append(entries []ClaimHistoryEntry) error {
	if h == nil || len(entries) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(h.path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue // ein kaputter Eintrag darf die anderen nicht verhindern
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// Read liefert die Einträge, neueste zuerst, höchstens limit viele.
// Eine fehlende Datei ist kein Fehler (noch nichts erhalten).
func (h *claimHistory) Read(limit int) ([]ClaimHistoryEntry, error) {
	if h == nil {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	f, err := os.Open(h.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ClaimHistoryEntry
	sc := bufio.NewScanner(f)
	// Belohnungsnamen sind kurz; der Default-Puffer (64 KB) reicht weit.
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e ClaimHistoryEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // kaputte Zeile überspringen statt alles zu verlieren
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Neueste zuerst: die Datei wird chronologisch fortgeschrieben, also
	// einfach umdrehen statt zu sortieren.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimHistory liefert die Erfolgsliste für die Weboberfläche,
// neueste zuerst. Öffentlich, weil der Web-Server sie braucht.
func (s *Service) ClaimHistory() ([]ClaimHistoryEntry, error) {
	return s.history.Read(claimHistoryMax)
}
