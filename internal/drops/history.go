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

// Received-drops history ("Received")
//
// A slim, append-only list of every drop we've ever received — one line per
// drop, forever. Purpose is purely informational: being able to look up what
// we got and when, even long after the campaign has vanished from Twitch.
//
// Deliberately separate from the claim record in the config (see
// localclaims.go):
//   - The record is working material, needs to be fast to look up, and is
//     pruned after 90 days. It only holds the ID and date.
//   - This list is for viewing and may stay forever. It holds the names that
//     the record doesn't — without them all we'd have is meaningless IDs like
//     "40edc785-8645-11f1-…".
//
// Why a dedicated file at all, when claims also show up in the debug log:
// the daily logs are ~5 MB per day, 99.9% noise, and get deleted after 14
// days. The received-drops history only grows by a few lines a day.
//
// Format: JSON Lines (one JSON line per entry). That makes appending a
// simple write; an interrupted write corrupts at most the last line, and the
// reader just skips broken lines.

// claimHistoryFile is the filename next to config.json. Deliberately NOT in
// the logs directory: the retention limit there cleans things up.
const claimHistoryFile = "claimed-history.jsonl"

// claimHistoryMax caps how many entries the web UI receives.
// The file itself is never truncated.
const claimHistoryMax = 2000

// ClaimHistoryEntry is a received drop.
type ClaimHistoryEntry struct {
	At       time.Time `json:"at"`       // when we first observed the claim
	Game     string    `json:"game"`     // e.g. "Marbles on Stream"
	Campaign string    `json:"campaign"` // e.g. "MarbleFest - July'26-Day2"
	Reward   string    `json:"reward"`   // e.g. "30 Tournament Coins"
	DropID   string    `json:"drop_id"`  // for tracing in the log
	Source   string    `json:"source"`   // "auto" = self-claimed, "extern" = claimed on Twitch
}

// claimHistory writes and reads the received-drops history. The mutex
// guards against concurrent appends (drops cycle) and reads (web UI).
type claimHistory struct {
	mu   sync.Mutex
	path string
}

// newClaimHistory places the list next to the given config file.
func newClaimHistory(configPath string) *claimHistory {
	dir := filepath.Dir(configPath)
	if dir == "" || dir == "." {
		dir = "config"
	}
	return &claimHistory{path: filepath.Join(dir, claimHistoryFile)}
}

// ensureWritable checks at startup whether the list can be created or
// appended to, and creates it right away in the process.
//
// It exists because a permission problem would otherwise only surface at
// the next claim — which can be hours later. Exactly this happened on
// 2026-07-28: the file had been created externally with the wrong owner,
// the error ("permission denied") only showed up in the log four hours
// later, and three received drops were missing from the list.
//
// The pitfall behind it: the container runs as root, but the stack sets
// `cap_drop: ALL`. That strips root of CAP_DAC_OVERRIDE too — it can no
// longer ignore file permissions and gets treated like a regular user. A
// file owned by someone else with only mode 644 is then unwritable, even
// though "root" is printed on the label.
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

// Append adds entries. Errors are not fatal — a missing received-drops
// history must not interrupt farming, so the caller at most logs the
// error.
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
			continue // a broken entry must not prevent the others
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// Read returns the entries, newest first, at most limit many.
// A missing file is not an error (nothing received yet).
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
	// Reward names are short; the default buffer (64 KB) is more than enough.
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e ClaimHistoryEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // skip broken line instead of losing everything
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Newest first: the file is appended to chronologically, so just
	// reverse instead of sorting.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimHistory returns the received-drops history for the web UI,
// newest first. Exported because the web server needs it.
func (s *Service) ClaimHistory() ([]ClaimHistoryEntry, error) {
	return s.history.Read(claimHistoryMax)
}
