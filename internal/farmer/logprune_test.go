package farmer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneOldLogs decides based on real files in a disposable directory.
// Most important point: it must ONLY match its own filename pattern —
// nothing the user placed in the logs directory themselves should
// disappear.
func TestPruneOldLogs(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	if err := os.Mkdir("logs", 0755); err != nil {
		t.Fatal(err)
	}

	stamp := func(daysAgo int) string {
		return time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02")
	}

	// name → should survive?
	files := map[string]bool{
		"debug-" + stamp(0) + ".log":  true,  // today
		"debug-" + stamp(13) + ".log": true,  // just within
		"debug-" + stamp(14) + ".log": true,  // exact boundary: 14 days old stays
		"debug-" + stamp(15) + ".log": false, // too old
		"debug-" + stamp(90) + ".log": false, // way too old
		// Foreign files: must stay untouched, no matter how old.
		"debug-" + stamp(90) + ".log.bak": true,
		"notizen.txt":                     true,
		"claimed-history.jsonl":           true,
		"debug-kaputt.log":                true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join("logs", name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	pruneOldLogs(nil)

	for name, shouldSurvive := range files {
		_, err := os.Stat(filepath.Join("logs", name))
		exists := err == nil
		if exists != shouldSurvive {
			if shouldSurvive {
				t.Errorf("%q should have stayed but is gone", name)
			} else {
				t.Errorf("%q should have been deleted but still exists", name)
			}
		}
	}
}

// TestPruneOldLogs_NoLogsDir: without a logs directory, nothing must happen
// and nothing must crash (first start, before the directory was created).
func TestPruneOldLogs_NoLogsDir(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	pruneOldLogs(nil) // must not panic
}
