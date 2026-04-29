//go:build !debug

package farmer

import "fmt"

// debugLog writes per-cycle/heartbeat noise (prober ticks, drops watch
// updates, raw WS payloads) to the daily debug log file only — silent in
// the live UI feed.
//
// Build with `-tags=debug` to surface these in the UI instead — useful
// for diagnosing pick/credit issues, otherwise too chatty.
func (f *Farmer) debugLog(format string, args ...any) {
	f.writeLogFile(fmt.Sprintf(format, args...))
}
