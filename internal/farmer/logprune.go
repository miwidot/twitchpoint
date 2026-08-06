package farmer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// logRetentionDays is the retention period for daily debug logs.
//
// There used to be none: a 2026-07-28 finding showed 127 MB in 11 days
// (~5 MB per day, 99.9% noise) — on a Raspberry Pi with a single SSD that
// also hosts DNS, Home Assistant, and the Git hosting. 14 days is plenty for
// troubleshooting; anything meant to be kept long-term belongs in the
// received-drops history (see internal/drops/history.go), not the debug log.
const logRetentionDays = 14

// debugLogPattern matches exactly the files we create ourselves. Deliberately
// strict: a glob like "debug-*" would also catch manually created copies
// ("debug-2026-07-23.log.bak"), and nothing the user deliberately placed in
// the logs directory should disappear.
var debugLogPattern = regexp.MustCompile(`^debug-\d{4}-\d{2}-\d{2}\.log$`)

// pruneOldLogs deletes daily debug logs older than logRetentionDays.
//
// The decision is based on the DATE IN THE FILENAME, not the modification
// time: a file copy or a restore from backup resets the modification time,
// but the date in the name stays correct.
//
// logf may be nil — on day rollover the call already runs under the log
// mutex, so a write attempt would deadlock itself.
func pruneOldLogs(logf func(string)) {
	entries, err := os.ReadDir("logs")
	if err != nil {
		return // no logs directory: nothing to do
	}

	// Normalize to local calendar days: cutoff = midnight N days ago, and
	// the filename is parsed in the SAME timezone. Otherwise the time in
	// time.Now() vs. the date parsed as UTC midnight would delete a log
	// that's exactly N days old half a day too early (timezone-dependent).
	now := time.Now()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -logRetentionDays)
	removed, freed := 0, int64(0)

	for _, e := range entries {
		if e.IsDir() || !debugLogPattern.MatchString(e.Name()) {
			continue
		}
		// "debug-YYYY-MM-DD.log" → date
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
		logf(fmt.Sprintf("Log cleanup: removed %d file(s) older than %d days (%.1f MB freed)",
			removed, logRetentionDays, float64(freed)/(1024*1024)))
	}
}
