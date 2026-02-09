package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultConfigFile = "config.json"

// ChannelEntry holds per-channel config.
type ChannelEntry struct {
	Login    string `json:"login"`
	Priority int    `json:"priority"` // 1 = always watch, 2 = rotate (default)
}

// Config holds the application configuration.
type Config struct {
	AuthToken      string         `json:"auth_token"`
	Channels       []string       `json:"channels,omitempty"`        // legacy: simple list
	ChannelConfigs []ChannelEntry `json:"channel_configs,omitempty"` // new: with priority
	WebEnabled     bool           `json:"web_enabled"`               // enable web UI
	WebPort        int            `json:"web_port"`                  // web server port (default 8080)
	IrcEnabled     bool           `json:"irc_enabled"`               // enable IRC for viewer presence (default true)

	path string // file path, not serialized
}

// Load reads the config from the given path. If path is empty, uses the default.
// Returns a default config if the file doesn't exist.
// Auto-saves if migration adds new fields.
func Load(path string) (*Config, error) {
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			path = defaultConfigFile
		} else {
			path = filepath.Join(filepath.Dir(exe), defaultConfigFile)
		}
		// If executable-relative doesn't exist, fall back to CWD
		if _, err := os.Stat(path); os.IsNotExist(err) {
			path = defaultConfigFile
		}
	}

	cfg := &Config{
		path:       path,
		WebPort:    8080, // default
		IrcEnabled: true, // default
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// Parse raw JSON to detect missing fields
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.path = path

	// Set defaults if not in file
	if _, hasPort := raw["web_port"]; !hasPort {
		cfg.WebPort = 8080
	}
	if _, hasIrc := raw["irc_enabled"]; !hasIrc {
		cfg.IrcEnabled = true
	}

	// Migrate legacy channels and detect if new fields need to be written
	needsSave := cfg.migrate()

	// Check if new fields are missing from file
	_, hasWebEnabled := raw["web_enabled"]
	_, hasWebPort := raw["web_port"]
	_, hasIrcEnabled := raw["irc_enabled"]
	if !hasWebEnabled || !hasWebPort || !hasIrcEnabled {
		needsSave = true
	}

	// Auto-save to add new fields to existing config
	if needsSave {
		_ = cfg.Save() // ignore error, not critical
	}

	return cfg, nil
}

// migrate converts legacy Channels list to ChannelConfigs.
// Returns true if any changes were made.
func (c *Config) migrate() bool {
	if len(c.Channels) == 0 {
		return false
	}
	existing := make(map[string]bool)
	for _, cc := range c.ChannelConfigs {
		existing[cc.Login] = true
	}
	for _, login := range c.Channels {
		login = strings.ToLower(login)
		if !existing[login] {
			c.ChannelConfigs = append(c.ChannelConfigs, ChannelEntry{
				Login:    login,
				Priority: 2, // default to rotation
			})
		}
	}
	c.Channels = nil // clear legacy field
	return true
}

// Save writes the config back to disk.
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(c.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// Path returns the config file path.
func (c *Config) Path() string {
	return c.path
}

// GetChannelLogins returns all channel logins.
func (c *Config) GetChannelLogins() []string {
	logins := make([]string, len(c.ChannelConfigs))
	for i, cc := range c.ChannelConfigs {
		logins[i] = cc.Login
	}
	return logins
}

// GetPriority returns the priority for a channel (1 or 2). Returns 2 if not found.
func (c *Config) GetPriority(login string) int {
	login = strings.ToLower(login)
	for _, cc := range c.ChannelConfigs {
		if cc.Login == login {
			if cc.Priority == 1 {
				return 1
			}
			return 2
		}
	}
	return 2
}

// SetPriority sets the priority for a channel.
func (c *Config) SetPriority(login string, priority int) bool {
	login = strings.ToLower(login)
	for i, cc := range c.ChannelConfigs {
		if cc.Login == login {
			c.ChannelConfigs[i].Priority = priority
			return true
		}
	}
	return false
}

// AddChannel adds a channel if not already present.
func (c *Config) AddChannel(login string) bool {
	login = strings.ToLower(login)
	for _, cc := range c.ChannelConfigs {
		if cc.Login == login {
			return false
		}
	}
	c.ChannelConfigs = append(c.ChannelConfigs, ChannelEntry{
		Login:    login,
		Priority: 2,
	})
	return true
}

// RemoveChannel removes a channel. Returns true if found and removed.
func (c *Config) RemoveChannel(login string) bool {
	login = strings.ToLower(login)
	for i, cc := range c.ChannelConfigs {
		if cc.Login == login {
			c.ChannelConfigs = append(c.ChannelConfigs[:i], c.ChannelConfigs[i+1:]...)
			return true
		}
	}
	return false
}

// HasChannel checks if a channel is in the config.
func (c *Config) HasChannel(login string) bool {
	login = strings.ToLower(login)
	for _, cc := range c.ChannelConfigs {
		if cc.Login == login {
			return true
		}
	}
	return false
}
