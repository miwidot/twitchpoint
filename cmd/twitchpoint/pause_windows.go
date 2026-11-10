//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// shouldPauseOnExit reports whether a fatal exit should wait for Enter.
//
// True only when stdin is an actual console handle, which is the case for a
// double-clicked exe (the console closes with the process, taking the error
// message with it) and for a user running the exe from cmd/PowerShell.
//
// GetConsoleMode is the authoritative test: it succeeds only for a console
// input handle and fails for a pipe, a redirected file, NUL and a missing
// stdin — exactly the unattended cases (Windows service, scheduled task,
// CI) where blocking on a read would hang the process forever instead of
// letting it exit.
func shouldPauseOnExit() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
