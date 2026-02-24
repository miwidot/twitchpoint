//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	autoStartKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartValue = "TwitchPoint"
)

func isAutoStartEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()

	_, _, err = key.GetStringValue(autoStartValue)
	return err == nil
}

func toggleAutoStart() (enabled bool, err error) {
	if isAutoStartEnabled() {
		// Remove
		key, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.SET_VALUE)
		if err != nil {
			return false, err
		}
		defer key.Close()
		err = key.DeleteValue(autoStartValue)
		return false, err
	}

	// Add
	exePath, err := os.Executable()
	if err != nil {
		return false, err
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	err = key.SetStringValue(autoStartValue, exePath)
	return err == nil, err
}
