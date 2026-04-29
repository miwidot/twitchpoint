package drops

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// CheckLoop polls the drops inventory periodically as a safety net for
// missed WebSocket events. v1.8.0 reduced the cadence from 5 to 15 min
// because user-drop-events PubSub now delivers progress in real-time.
//
// Pass the farmer's stop channel so the loop exits at shutdown.
func (s *Service) CheckLoop(stopCh <-chan struct{}) {
	// Initial check shortly after startup (give channels time to initialize).
	timer := time.NewTimer(30 * time.Second)
	select {
	case <-timer.C:
		s.ProcessDrops()
	case <-stopCh:
		timer.Stop()
		return
	}

	ticker := time.NewTicker(15 * time.Minute) // v1.8.0: WebSocket carries the real-time load
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.ProcessDrops()
		case <-stopCh:
			return
		}
	}
}

// ProcessDrops fetches the inventory, runs the selector, applies the
// pick, and auto-claims any completed drops. Called every 15 min by
// CheckLoop, plus out-of-band from PollProgressOnce (when poll says
// the current drop hit 100%), HandleDropClaim (after a claim settles),
// HandleGameChange (after a debounced wrong-game decision), and
// SetCampaignEnabled (when the user toggles a campaign).
func (s *Service) ProcessDrops() {
	campaigns, err := s.gql.GetDropsInventory()
	if err != nil {
		s.log("[Drops] Failed to fetch inventory: %v", err)
		return
	}

	if s.writeLogFile != nil {
		s.writeLogFile(fmt.Sprintf("[Drops] Inventory returned %d campaigns", len(campaigns)))
	}

	// 1. Auto-claim any drops that are complete and have an instance ID.
	s.AutoClaimAndMarkCompleted(campaigns)

	// 2a. Compare the previous pick's drop progress against this cycle's
	//     inventory. If Twitch did not credit any new minutes, put the
	//     channel into stall cooldown so the selector skips it next time.
	s.Stall.Apply(campaigns)

	// 2b. Build the active skip-set from the cooldown map (filtering
	//     expired entries).
	skipChannels := s.Stall.ActiveSkipSet()

	// 2c. Run the selector on the (now-updated) inventory, with stalled
	//     channels skipped.
	pick, pool := s.Selector.Select(campaigns, skipChannels)

	// 3. Build per-campaign UI rows.
	active, queued, idle := BuildRows(s.cfg, campaigns, pick, pool)

	// 4. Rebuild campaign cache (for web UI endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 5. Apply pick: register channel as temp if new, set HasActiveDrop.
	pickApplied := s.ApplyPick(pick, campaigns)

	// 6. If the pick was NOT applied (refresh failed for the new pick),
	//    bail out without committing anything — rows, currentPickID,
	//    cleanup, and snapshot all use the previous pick's state which
	//    is still valid. Next cycle will retry with fresh inventory.
	if !pickApplied {
		s.log("[Drops/Pool] skipping commit — pick refresh failed, previous state preserved")
		return
	}

	// 7. Pick applied successfully — commit the new state atomically.
	s.mu.Lock()
	s.ActiveDrops = active
	s.QueuedDrops = queued
	s.IdleDrops = idle
	s.CampaignCache = newCache
	if pick != nil {
		s.CurrentPickID = pick.ChannelID
	} else {
		s.CurrentPickID = ""
	}
	s.mu.Unlock()

	// 8. Drop existing temp channels that are no longer the pick.
	s.CleanupNonPickedTemps(pick)

	// 9. Trigger the points-side Spade rotation so slot 1 reflects the
	//    new pick (rotation lives in farmer; Service can't reach in).
	if s.triggerRotation != nil {
		s.triggerRotation()
	}

	if pick != nil {
		campaignNames := make([]string, len(pick.Campaigns))
		for i, c := range pick.Campaigns {
			campaignNames[i] = c.Name
		}
		s.log("[Drops/Pool] picked %s (campaigns: %s)", pick.DisplayName, strings.Join(campaignNames, ", "))
	} else {
		s.log("[Drops/Pool] empty pool — drops idle, slots free for points")
	}

	// 10. Snapshot the picked channel's current drop progress so the
	//     next cycle can detect whether Twitch credited any minutes.
	s.Stall.SnapshotPick(pick, campaigns)
}

// GetActiveDrops returns a single concatenated slice of UI rows in
// display order: ACTIVE / DISABLED / COMPLETED first, then QUEUED, then
// IDLE.
func (s *Service) GetActiveDrops() []ActiveDrop {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := len(s.ActiveDrops) + len(s.QueuedDrops) + len(s.IdleDrops)
	if total == 0 {
		return nil
	}
	out := make([]ActiveDrop, 0, total)
	out = append(out, s.ActiveDrops...)
	out = append(out, s.QueuedDrops...)
	out = append(out, s.IdleDrops...)
	return out
}

// GetEligibleGames returns the unique sorted list of game names from
// the current cycle's inventory cache. Used as the default
// autocomplete pool for the "wanted games" UI; for free-text search
// across ALL Twitch categories, callers should additionally hit
// SearchGameCategories.
func (s *Service) GetEligibleGames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]bool)
	var out []string
	for _, c := range s.CampaignCache {
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
