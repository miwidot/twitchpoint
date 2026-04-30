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
//
// Serialized via processMu+TryLock+rerunRequested: only one full
// inventory→selector→apply→commit cycle runs at a time. Concurrent
// triggers during a run flag rerunRequested, and ONE extra pass fires
// after the current run completes (so newly-enabled campaigns or
// streamer state changes that arrived mid-run don't get lost). The
// goroutine spawn at the end is intentional — it lets the current
// caller's defer-Unlock land BEFORE the next pass starts, so the
// piggyback run sees a clean lock state.
func (s *Service) ProcessDrops() {
	if !s.processMu.TryLock() {
		// Another ProcessDrops is mid-flight. Flag a rerun so the
		// in-flight run picks up our trigger when it finishes.
		s.rerunRequested.Store(true)
		return
	}
	defer func() {
		// Read the rerun flag while we still hold the lock — that way
		// no other caller can have started a "fresh" run between our
		// unlock and our swap (which would be benign but spawns an
		// extra rerun goroutine for no useful work).
		rerun := s.rerunRequested.Swap(false)
		s.processMu.Unlock()
		if rerun {
			go s.ProcessDrops()
		}
	}()

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

	// 2b–2d. Select + apply with bounded retry. ApplyPick can reject the
	//        pick for recoverable reasons (game-mismatch, id-mismatch)
	//        — both set a manual cooldown on the bad channel. Without
	//        the retry, the wrong pick stays visible in UI/Watcher
	//        state until the next ProcessDrops trigger (could be 15
	//        min if no other path fires). With the retry, we re-fetch
	//        the skip-set + Selector.Select with the updated cooldown
	//        and try the next-best candidate inline.
	const maxApplyRetries = 3
	var pick *PoolEntry
	var pool []*PoolEntry
	applied := false

	for attempt := 0; attempt < maxApplyRetries; attempt++ {
		skipChannels := s.Stall.ActiveSkipSet()
		pick, pool = s.Selector.Select(campaigns, skipChannels)

		switch s.ApplyPick(pick, campaigns) {
		case ApplyApplied:
			applied = true
		case ApplyRetry:
			// Cooldown was set on the rejected channel; the next
			// Stall.ActiveSkipSet() call will include it. Loop back
			// to re-select.
			s.log("[Drops/Pool] pick rejected (retry %d/%d); re-selecting with updated skip-set",
				attempt+1, maxApplyRetries)
			continue
		case ApplyBail:
			s.log("[Drops/Pool] skipping commit — pick refresh failed, previous state preserved")
			return
		}
		break
	}

	if !applied {
		s.log("[Drops/Pool] all %d retry attempts hit retryable failures; preserving previous state",
			maxApplyRetries)
		return
	}

	// 3. Build per-campaign UI rows from the FINAL committed pick.
	active, queued, idle := BuildRows(s.cfg, campaigns, pick, pool)

	// 4. Rebuild campaign cache (for web UI endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 7. Pick applied successfully — commit the new state atomically.
	s.mu.Lock()
	s.activeDrops = active
	s.queuedDrops = queued
	s.idleDrops = idle
	s.campaignCache = newCache
	if pick != nil {
		s.currentPickID = pick.ChannelID
	} else {
		s.currentPickID = ""
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

	total := len(s.activeDrops) + len(s.queuedDrops) + len(s.idleDrops)
	if total == 0 {
		return nil
	}
	out := make([]ActiveDrop, 0, total)
	out = append(out, s.activeDrops...)
	out = append(out, s.queuedDrops...)
	out = append(out, s.idleDrops...)
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
	for _, c := range s.campaignCache {
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
