package farmer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneOldLogs entscheidet über echte Dateien in einem Wegwerf-Verzeichnis.
// Wichtigster Punkt: Es darf NUR das eigene Namensmuster treffen — im
// logs-Verzeichnis soll nichts verschwinden, was der Nutzer dort selbst
// abgelegt hat.
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

	// name → soll überleben?
	files := map[string]bool{
		"debug-" + stamp(0) + ".log":  true,  // heute
		"debug-" + stamp(13) + ".log": true,  // knapp innerhalb
		"debug-" + stamp(14) + ".log": true,  // exakte Grenze: 14 Tage alt bleibt
		"debug-" + stamp(15) + ".log": false, // zu alt
		"debug-" + stamp(90) + ".log": false, // deutlich zu alt
		// Fremde Dateien: müssen unangetastet bleiben, egal wie alt.
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
				t.Errorf("%q hätte bleiben müssen, ist aber weg", name)
			} else {
				t.Errorf("%q hätte gelöscht werden müssen, existiert aber noch", name)
			}
		}
	}
}

// TestPruneOldLogs_NoLogsDir: ohne logs-Verzeichnis darf nichts passieren und
// nichts krachen (erster Start, bevor das Verzeichnis angelegt wurde).
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

	pruneOldLogs(nil) // darf nicht panicken
}
