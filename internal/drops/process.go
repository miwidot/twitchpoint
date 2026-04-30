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

// ProcessDrops kicks an inventory→selector→apply→commit cycle.
// It's a non-blocking enqueue: if the worker is busy, the trigger
// is coalesced with the already-queued kick (the worker re-fetches
// fresh inventory on each pass, so multiple triggers during one
// run all want "another pass with fresh data" — exactly one is
// enough).
//
// Trigger sources: 15-min CheckLoop ticker, 60s PollProgressOnce
// silent-pick path, HandleDropClaim, HandleGameChange (after the
// 30s debounce), EventStreamDown when the picked drop channel
// goes offline, and SetCampaignEnabled from TUI/Web toggles.
//
// Replaces the previous TryLock+rerunRequested design which had a
// Swap-vs-Unlock TOCTOU race that could lose triggers fired
// between the deferred Swap and the deferred Unlock.
func (s *Service) ProcessDrops() {
	select {
	case s.processQueue <- struct{}{}:
		// Trigger queued; worker will pick it up.
	default:
		// Worker is already running OR has a kick queued — the
		// in-flight + the queued kick together cover this trigger.
	}
}

// ProcessLoop drains processQueue, running one processOnce per
// kick. Started as a goroutine by farmer.Start — there's exactly
// one of these alive per Service, and processOnce is therefore
// the single point of inventory mutation. No lock needed inside
// processOnce because no other goroutine can be in it.
func (s *Service) ProcessLoop(stopCh <-chan struct{}) {
	for {
		select {
		case <-s.processQueue:
			s.processOnce()
		case <-stopCh:
			return
		}
	}
}

// processOnce is the actual inventory→selector→apply→commit body.
// Only ProcessLoop calls this, exactly one at a time, so it can
// freely mutate s.activeDrops/queuedDrops/idleDrops/campaignCache/
// currentPickID without an outer process-lock (the per-field
// s.mu still serializes UI reads vs commit writes).
func (s *Service) processOnce() {
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
