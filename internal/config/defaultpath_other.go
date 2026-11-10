//go:build !windows

package config

// newConfigPath keeps the historical behaviour on Linux/macOS: create the
// config in the current working directory.
func newConfigPath() string { return defaultConfigFile }
