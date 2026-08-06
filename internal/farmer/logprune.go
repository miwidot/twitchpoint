package farmer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// logRetentionDays ist die Aufbewahrungsdauer der Tages-Debug-Logs.
//
// Vorher gab es keine: Befund vom 28.07.2026 waren 127 MB in 11 Tagen (~5 MB
// pro Tag, zu 99,9 % Rauschen) — auf einem Raspberry Pi mit einer einzigen SSD,
// auf der auch DNS, Home Assistant und das Git-Hosting liegen. 14 Tage reichen
// für die Fehlersuche bequem; alles, was man dauerhaft behalten will, gehört in
// die Erfolgsliste (siehe internal/drops/history.go), nicht ins Debug-Log.
const logRetentionDays = 14

// debugLogPattern trifft genau die Dateien, die wir selbst anlegen. Absichtlich
// streng: ein Glob wie "debug-*" würde auch von Hand angelegte Kopien
// ("debug-2026-07-23.log.bak") erwischen, und im logs-Verzeichnis soll nichts
// verschwinden, was der Nutzer dort bewusst abgelegt hat.
var debugLogPattern = regexp.MustCompile(`^debug-\d{4}-\d{2}-\d{2}\.log$`)

// pruneOldLogs löscht Tages-Debug-Logs, die älter als logRetentionDays sind.
//
// Entschieden wird nach dem DATUM IM DATEINAMEN, nicht nach der
// Änderungszeit: Ein Dateikopiervorgang oder eine Wiederherstellung aus dem
// Backup setzt die Änderungszeit neu, das Datum im Namen bleibt korrekt.
//
// logf darf nil sein — beim Tageswechsel läuft der Aufruf schon unter dem
// Log-Mutex, ein Schreibversuch würde sich selbst blockieren.
func pruneOldLogs(logf func(string)) {
	entries, err := os.ReadDir("logs")
	if err != nil {
		return // kein logs-Verzeichnis: nichts zu tun
	}

	// Auf lokale Kalendertage normieren: cutoff = Mitternacht vor N Tagen,
	// und der Dateiname wird in DERSELBEN Zeitzone geparst. Sonst würde die
	// Uhrzeit in time.Now() vs. das als UTC-Mitternacht geparste Datum ein
	// exakt N Tage altes Log den halben Tag zu früh löschen (zeitzonenabhängig).
	now := time.Now()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -logRetentionDays)
	removed, freed := 0, int64(0)

	for _, e := range entries {
		if e.IsDir() || !debugLogPattern.MatchString(e.Name()) {
			continue
		}
		// "debug-YYYY-MM-DD.log" → Datum
		day, err := time.ParseInLocation("2006-01-02", e.Name()[6:16], now.Location())
		if err != nil || !day.Before(cutoff) {
			continue
		}
		path := filepath.Join("logs", e.Name())
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		if os.Remove(path) == nil {
			removed++
			freed += size
		}
	}

	if removed > 0 && logf != nil {
		logf(fmt.Sprintf("Log-Aufräumen: %d Datei(en) älter als %d Tage gelöscht (%.1f MB frei)",
			removed, logRetentionDays, float64(freed)/(1024*1024)))
	}
}
