package farmer

import (
	"fmt"

	"github.com/miwi/twitchpoint/internal/drops"
)

// SetCampaignEnabled enables or disables a drop campaign and triggers an
// immediate inventory re-evaluation so the selector picks up the change.
func (f *Farmer) SetCampaignEnabled(campaignID string, enabled bool) error {
	f.cfg.SetCampaignEnabled(campaignID, enabled)
	if err := f.cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if enabled {
		f.addLog("[Drops] Enabled campaign %s", campaignID)
	} else {
		f.addLog("[Drops] Disabled campaign %s", campaignID)
	}

	go f.drops.ProcessDrops()
	return nil
}

// GetActiveDrops returns drop UI rows in display order — public API
// surface used by the web /api/drops endpoint and the TUI.
func (f *Farmer) GetActiveDrops() []drops.ActiveDrop {
	return f.drops.GetActiveDrops()
}

// GetEligibleGames returns the unique sorted list of game names from
// the current cycle's inventory cache. Used as the default
// autocomplete pool for the wanted-games UI.
func (f *Farmer) GetEligibleGames() []string {
	return f.drops.GetEligibleGames()
}

// SearchGameCategories proxies to Twitch's searchCategories GQL — used
// by the web/TUI autocomplete to resolve game names that aren't in the
// user's current inventory. Returns up to `limit` matching game name
// strings.
func (f *Farmer) SearchGameCategories(query string, limit int) ([]string, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	return f.gql.SearchGameCategories(query, limit)
}
