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
	ID       string `json:"id,omitempty"` // Twitch channel ID (persisted, survives renames)
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
	DropsEnabled          bool     `json:"drops_enabled"`                         // enable drop mining (default true)
	DisabledCampaigns     []string `json:"disabled_campaigns,omitempty"`          // campaign IDs to skip
	CompletedCampaigns    []string `json:"completed_campaigns,omitempty"`         // campaign IDs already fully claimed
	PinnedCampaignID      string   `json:"pinned_campaign_id,omitempty"`          // v1.7.0 (deprecated v1.8.0; ignored by selector but kept for backward compat)
	GamesToWatch          []string `json:"games_to_watch,omitempty"`              // v1.8.0 ordered priority list of game names; empty = remaining_time fallback

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
		path:         path,
		WebEnabled:   true, // default
		WebPort:      8080, // default
		IrcEnabled:   true, // default
		DropsEnabled: true, // default
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
	if _, hasWebEnabled := raw["web_enabled"]; !hasWebEnabled {
		cfg.WebEnabled = true
	}
	if _, hasPort := raw["web_port"]; !hasPort {
		cfg.WebPort = 8080
	}
	if _, hasIrc := raw["irc_enabled"]; !hasIrc {
		cfg.IrcEnabled = true
	}
	if _, hasDrops := raw["drops_enabled"]; !hasDrops {
		cfg.DropsEnabled = true
	}

	// Migrate legacy channels and detect if new fields need to be written
	needsSave := cfg.migrate()

	// Remove stale exclusive_drops field from config
	if _, hasExclusiveDrops := raw["exclusive_drops"]; hasExclusiveDrops {
		needsSave = true
	}

	// Check if new fields are missing from file
	_, hasWebEnabled := raw["web_enabled"]
	_, hasWebPort := raw["web_port"]
	_, hasIrcEnabled := raw["irc_enabled"]
	_, hasDropsEnabled := raw["drops_enabled"]
	if !hasWebEnabled || !hasWebPort || !hasIrcEnabled || !hasDropsEnabled {
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

// GetChannelEntries returns all channel entries (with ID, login, priority).
func (c *Config) GetChannelEntries() []ChannelEntry {
	result := make([]ChannelEntry, len(c.ChannelConfigs))
	copy(result, c.ChannelConfigs)
	return result
}

// UpdateChannelLogin updates the login for a channel identified by ID.
// Returns true if the channel was found and updated.
func (c *Config) UpdateChannelLogin(channelID, newLogin string) bool {
	for i, cc := range c.ChannelConfigs {
		if cc.ID == channelID {
			c.ChannelConfigs[i].Login = newLogin
			return true
		}
	}
	return false
}

// SetChannelID sets the ID for a channel identified by login.
func (c *Config) SetChannelID(login, channelID string) {
	login = strings.ToLower(login)
	for i, cc := range c.ChannelConfigs {
		if cc.Login == login {
			c.ChannelConfigs[i].ID = channelID
			return
		}
	}
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

// IsCampaignDisabled returns true if the campaign ID is in the disabled list.
func (c *Config) IsCampaignDisabled(campaignID string) bool {
	for _, id := range c.DisabledCampaigns {
		if id == campaignID {
			return true
		}
	}
	return false
}

// SetCampaignEnabled adds or removes a campaign ID from the disabled list.
// enabled=true removes from disabled, enabled=false adds to disabled.
func (c *Config) SetCampaignEnabled(campaignID string, enabled bool) {
	if enabled {
		// Remove from disabled list
		for i, id := range c.DisabledCampaigns {
			if id == campaignID {
				c.DisabledCampaigns = append(c.DisabledCampaigns[:i], c.DisabledCampaigns[i+1:]...)
				return
			}
		}
	} else {
		// Add to disabled list if not already present
		if !c.IsCampaignDisabled(campaignID) {
			c.DisabledCampaigns = append(c.DisabledCampaigns, campaignID)
		}
	}
}

// IsCampaignCompleted checks if a campaign has been fully claimed.
func (c *Config) IsCampaignCompleted(campaignID string) bool {
	for _, id := range c.CompletedCampaigns {
		if id == campaignID {
			return true
		}
	}
	return false
}

// MarkCampaignCompleted adds a campaign ID to the completed list.
func (c *Config) MarkCampaignCompleted(campaignID string) {
	if !c.IsCampaignCompleted(campaignID) {
		c.CompletedCampaigns = append(c.CompletedCampaigns, campaignID)
	}
}

// SetPinnedCampaign atomically sets the pinned campaign. Pass empty string to clear.
// Only one campaign can be pinned at a time — calling this overwrites any previous pin.
func (c *Config) SetPinnedCampaign(campaignID string) {
	c.PinnedCampaignID = campaignID
}

// IsCampaignPinned returns true if the given campaign is the currently pinned one.
func (c *Config) IsCampaignPinned(campaignID string) bool {
	return c.PinnedCampaignID != "" && c.PinnedCampaignID == campaignID
}

// GetPinnedCampaign returns the currently pinned campaign ID, or empty string if none.
func (c *Config) GetPinnedCampaign() string {
	return c.PinnedCampaignID
}

// GetGamesToWatch returns the ordered wanted-games list (copy, safe to mutate).
func (c *Config) GetGamesToWatch() []string {
	out := make([]string, len(c.GamesToWatch))
	copy(out, c.GamesToWatch)
	return out
}

// AddGameToWatch appends a game to the end of the priority list if not already present (case-insensitive).
func (c *Config) AddGameToWatch(game string) {
	game = strings.TrimSpace(game)
	if game == "" {
		return
	}
	for _, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			return
		}
	}
	c.GamesToWatch = append(c.GamesToWatch, game)
}

// RemoveGameFromWatch removes a game from the priority list (case-insensitive).
func (c *Config) RemoveGameFromWatch(game string) {
	for i, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			c.GamesToWatch = append(c.GamesToWatch[:i], c.GamesToWatch[i+1:]...)
			return
		}
	}
}

// MoveGameToWatch shifts a game one position in the priority list.
// direction: -1 = up (toward higher priority), +1 = down. No-op at boundaries.
func (c *Config) MoveGameToWatch(game string, direction int) {
	idx := -1
	for i, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	target := idx + direction
	if target < 0 || target >= len(c.GamesToWatch) {
		return
	}
	c.GamesToWatch[idx], c.GamesToWatch[target] = c.GamesToWatch[target], c.GamesToWatch[idx]
}

// SetGamesToWatch replaces the whole list (used by web API atomic reorder).
// Trims whitespace, dedupes case-insensitively, drops empty entries.
func (c *Config) SetGamesToWatch(games []string) {
	out := make([]string, 0, len(games))
	seen := make(map[string]bool, len(games))
	for _, g := range games {
		key := strings.ToLower(strings.TrimSpace(g))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, strings.TrimSpace(g))
	}
	c.GamesToWatch = out
}

// UnmarkCampaignCompleted removes a campaign ID from the completed list.
// Used by daily-rolling-campaign scrub when Twitch resets a campaign's drops.
func (c *Config) UnmarkCampaignCompleted(campaignID string) {
	for i, id := range c.CompletedCampaigns {
		if id == campaignID {
			c.CompletedCampaigns = append(c.CompletedCampaigns[:i], c.CompletedCampaigns[i+1:]...)
			return
		}
	}
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
