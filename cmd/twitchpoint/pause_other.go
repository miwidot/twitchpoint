//go:build !windows

package main

// shouldPauseOnExit is Windows-only behaviour: on Linux and macOS the shell
// that launched the binary keeps the output visible after it exits, so a
// fatal error never needs to wait for a keypress.
func shouldPauseOnExit() bool { return false }
