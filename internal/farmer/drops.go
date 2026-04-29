package farmer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// dropCheckLoop polls the drops inventory periodically as a safety net for
// missed WebSocket events. v1.8.0 reduced the cadence from 5 to 15 min because
// user-drop-events PubSub now delivers progress in real-time.
func (f *Farmer) dropCheckLoop() {
	// Initial check shortly after startup (give channels time to initialize)
	timer := time.NewTimer(30 * time.Second)
	select {
	case <-timer.C:
		f.processDrops()
	case <-f.stopCh:
		timer.Stop()
		return
	}

	ticker := time.NewTicker(15 * time.Minute) // v1.8.0: WebSocket carries the real-time load
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.processDrops()
		case <-f.stopCh:
			return
		}
	}
}


// processDrops fetches the inventory, runs the selector, applies the pick to
// Spade, and auto-claims any completed drops. Called every 5 min by dropCheckLoop.
func (f *Farmer) processDrops() {
	campaigns, err := f.gql.GetDropsInventory()
	if err != nil {
		f.addLog("[Drops] Failed to fetch inventory: %v", err)
		return
	}

	f.writeLogFile(fmt.Sprintf("[Drops] Inventory returned %d campaigns", len(campaigns)))

	// v1.8.0 had a scrubStaleCompleted call here that read dashboard's
	// claimed=false to un-mark completed campaigns. It fought our poll-based
	// completion (DropCurrentSessionContext: 744/720 = done) because Twitch's
	// dashboard often lies (claimed:false even after user claimed via web).
	// Removed: poll is the authoritative completion source now.

	// 1. Auto-claim any drops that are complete and have an instance ID.
	f.drops.AutoClaimAndMarkCompleted(campaigns)

	// 2a. Compare the previous pick's drop progress against this cycle's
	//     inventory. If Twitch did not credit any new minutes, put the channel
	//     into stallCooldown so the selector skips it next time.
	f.drops.Stall.Apply(campaigns)

	// 2b. Build the active skip-set from stallCooldown (filtering expired entries).
	skipChannels := f.drops.Stall.ActiveSkipSet()

	// 2c. Run the selector on the (now-updated) inventory, with stalled channels skipped.
	pick, pool := f.drops.Selector.Select(campaigns, skipChannels)

	// 3. Build per-campaign UI rows.
	active, queued, idle := drops.BuildRows(f.cfg, campaigns, pick, pool)

	// 4. Rebuild campaign cache (for web UI endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 5. Apply pick: register channel as temp if new, set HasActiveDrop.
	pickApplied := f.drops.ApplyPick(pick, campaigns)

	// 6. If the pick was NOT applied (refresh failed for the new pick), bail
	//    out without committing anything — rows, currentPickID, cleanup, and
	//    snapshot all use the previous pick's state which is still valid.
	//    Next cycle will retry with fresh inventory.
	if !pickApplied {
		f.addLog("[Drops/Pool] skipping commit — pick refresh failed, previous state preserved")
		return
	}

	// 7. Pick applied successfully — commit the new state atomically.
	f.drops.Lock()
	f.drops.ActiveDrops = active
	f.drops.QueuedDrops = queued
	f.drops.IdleDrops = idle
	f.drops.CampaignCache = newCache
	if pick != nil {
		f.drops.CurrentPickID = pick.ChannelID
	} else {
		f.drops.CurrentPickID = ""
	}
	f.drops.Unlock()

	// 8. Drop existing temp channels that are no longer the pick.
	f.drops.CleanupNonPickedTemps(pick)

	// 9. Trigger rotation so Spade slot 1 reflects the new pick.
	f.rotateChannels()

	if pick != nil {
		campaignNames := make([]string, len(pick.Campaigns))
		for i, c := range pick.Campaigns {
			campaignNames[i] = c.Name
		}
		f.addLog("[Drops/Pool] picked %s (campaigns: %s)", pick.DisplayName, strings.Join(campaignNames, ", "))
	} else {
		f.addLog("[Drops/Pool] empty pool — drops idle, slots free for points")
	}

	// 10. Snapshot the picked channel's current drop progress so the next cycle
	//     can detect whether Twitch credited any minutes.
	f.drops.Stall.SnapshotPick(pick, campaigns)
}

// SetCampaignEnabled enables or disables a drop campaign.
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

	// Trigger immediate re-evaluation
	go f.processDrops()
	return nil
}
// Order: ACTIVE / DISABLED / COMPLETED first, then QUEUED, then IDLE.
func (f *Farmer) GetActiveDrops() []drops.ActiveDrop {
	f.drops.RLock()
	defer f.drops.RUnlock()

	total := len(f.drops.ActiveDrops) + len(f.drops.QueuedDrops) + len(f.drops.IdleDrops)
	if total == 0 {
		return nil
	}
	out := make([]drops.ActiveDrop, 0, total)
	out = append(out, f.drops.ActiveDrops...)
	out = append(out, f.drops.QueuedDrops...)
	out = append(out, f.drops.IdleDrops...)
	return out
}

// GetEligibleGames returns the unique sorted list of game names from the
// current cycle's inventory (account's currently-known campaigns). Used as
// the default autocomplete pool. For free-text search across ALL Twitch
// categories (e.g. "tarkov" before any EFT campaign appears in inventory),
// callers should additionally hit /api/games/search backed by SearchGameCategories.
func (f *Farmer) GetEligibleGames() []string {
	f.drops.RLock()
	defer f.drops.RUnlock()

	seen := make(map[string]bool)
	var out []string
	for _, c := range f.drops.CampaignCache {
		if c.GameName == "" {
			continue
		}
		key := strings.ToLower(c.GameName)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c.GameName)
	}
	sort.Strings(out)
	return out
}

// SearchGameCategories proxies to Twitch's searchCategories GQL — used by the
// web/TUI autocomplete to resolve game names that aren't in the user's current
// inventory. Returns up to `limit` matching game name strings.
func (f *Farmer) SearchGameCategories(query string, limit int) ([]string, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	return f.gql.SearchGameCategories(query, limit)
}

// (scrubStaleCompleted was removed — it conflicted with the
// DropCurrentSession poll-based completion which is now authoritative.
// Daily-rolling campaigns where Twitch reuses the same ID across days
// will need manual un-disable via TUI 't' if they get stuck completed —
// in practice Twitch tends to issue new campaign IDs per day.)
