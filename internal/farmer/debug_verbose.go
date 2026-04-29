//go:build debug

package farmer

// debugLog (debug build) — same noise as the release path but routed
// through addLog so every probe/heartbeat shows up in the live UI feed.
// Build with `go build -tags=debug` to enable.
func (f *Farmer) debugLog(format string, args ...any) {
	f.addLog(format, args...)
}
