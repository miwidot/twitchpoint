//go:build windows

package config

import (
	"os"
	"path/filepath"
)

// newConfigPath is where a config is CREATED when none exists yet. An
// installed copy lives under Program Files, which a normal user cannot
// write to, so the config goes to %APPDATA%\twitchpoint instead.
func newConfigPath() string {
	if dir := os.Getenv("APPDATA"); dir != "" {
		return filepath.Join(dir, "twitchpoint", defaultConfigFile)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "twitchpoint", defaultConfigFile)
	}
	return defaultConfigFile
}
