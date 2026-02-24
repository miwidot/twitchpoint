//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	user32                 = syscall.NewLazyDLL("user32.dll")
	getConsoleWindow       = kernel32.NewProc("GetConsoleWindow")
	showWindowProc         = user32.NewProc("ShowWindow")
	setForeground          = user32.NewProc("SetForegroundWindow")
	isWindowVisible        = user32.NewProc("IsWindowVisible")
	setConsoleCtrlHandler  = kernel32.NewProc("SetConsoleCtrlHandler")
)

const (
	swHide         = 0
	swShow         = 5
	ctrlCloseEvent = 2
)

// setupConsoleCloseHandler intercepts the console X button (CTRL_CLOSE_EVENT).
// Instead of letting Windows terminate the process, we hide the console.
func setupConsoleCloseHandler() {
	handler := syscall.NewCallback(func(ctrlType uint32) uintptr {
		if ctrlType == ctrlCloseEvent {
			hideConsole()
			return 1 // handled — don't terminate
		}
		return 0 // pass through to default/TUI handler
	})
	setConsoleCtrlHandler.Call(handler, 1)
}

func getConsoleHWND() uintptr {
	hwnd, _, _ := getConsoleWindow.Call()
	return hwnd
}

func hideConsole() {
	hwnd := getConsoleHWND()
	if hwnd != 0 {
		showWindowProc.Call(hwnd, uintptr(swHide))
	}
}

func showConsoleWindow() {
	hwnd := getConsoleHWND()
	if hwnd != 0 {
		showWindowProc.Call(hwnd, uintptr(swShow))
		setForeground.Call(hwnd)
	}
}

func isConsoleVisible() bool {
	hwnd := getConsoleHWND()
	if hwnd == 0 {
		return false
	}
	ret, _, _ := isWindowVisible.Call(hwnd)
	return ret != 0
}

func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
