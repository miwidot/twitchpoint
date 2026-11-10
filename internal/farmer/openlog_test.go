package farmer

import (
	"os"
	"path/filepath"
	"testing"
)

// A working directory where ./logs cannot be created (here: a regular file
// named "logs" blocks it) must fall back to <config dir>/logs, not fail.
func TestOpenDebugLog_FallsBackToConfigDir(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "logs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	cfgDir := t.TempDir()

	dir, f := openDebugLog(filepath.Join(cfgDir, "config.json"))
	if f == nil {
		t.Fatal("expected fallback log file")
	}
	defer f.Close()
	if want := filepath.Join(cfgDir, "logs"); dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestOpenDebugLog_NoWritableDirReturnsNil(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "logs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "logs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, f := openDebugLog(filepath.Join(cfgDir, "config.json")); f != nil {
		f.Close()
		t.Fatal("expected nil when no directory is usable")
	}
}
