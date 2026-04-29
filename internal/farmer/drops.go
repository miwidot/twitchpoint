package farmer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// ActiveDrop represents a drop being tracked, exposed for the Web UI.
type ActiveDrop struct {
	CampaignID         string    `json:"campaign_id"`
	CampaignName       string    `json:"campaign_name"`
	GameName           string    `json:"game_name"`
	DropName           string    `json:"drop_name"`
	ChannelLogin       string    `json:"channel_login"`        // matched channel (if any)
	Progress           int       `json:"progress"`              // current minutes watched
	Required           int       `json:"required"`               // minutes required
	Percent            int       `json:"percent"`                // 0-100
	IsClaimed          bool      `json:"is_claimed"`
	EndAt              time.Time `json:"end_at"`                 // campaign end time
	IsAutoSelected     bool      `json:"is_auto_selected"`       // channel was auto-discovered
	IsEnabled          bool      `json:"is_enabled"`              // campaign not disabled
	IsAccountConnected bool      `json:"is_account_connected"`   // account linked for this game
	Status             string    `json:"status"`                 // ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED
	IsPinned           bool      `json:"is_pinned"`
	QueueIndex         int       `json:"queue_index"`            // 1-based for ACTIVE/QUEUED/IDLE; 0 otherwise
	EtaMinutes         int       `json:"eta_minutes"`            // RequiredMinutesWatched - CurrentMinutesWatched of next-to-claim drop
}

// stallCooldownDuration is how long a channel is excluded from the pool
// after Twitch failed to credit drop progress for that channel for one cycle.
// 30 min ≈ 6 cycles — long enough that we exhaust other candidates before
// retrying, short enough to recover from temporary Twitch hiccups.
const stallCooldownDuration = 30 * time.Minute

// cooldownReason explains why a channel is currently in cooldown. It matters
// because applyStallDetection clears cooldowns when a channel credits new
// minutes — but that recovery logic must NOT clear cooldowns set deliberately
// by other paths (game change, ID mismatch) that have their own expiry.
type cooldownReason int

const (
	// cooldownStall — set by applyStallDetection when no minutes were credited
	// in the previous cycle. Clearable by applyStallDetection on credit.
	cooldownStall cooldownReason = iota
	// cooldownManual — set deliberately by handleChannelGameChange or the
	// applySelectorPick guards (wrong game, id mismatch). NOT clearable by
	// applyStallDetection — only the timeout removes them.
	cooldownManual
)

type cooldownEntry struct {
	expires time.Time
	reason  cooldownReason
}

// dropState holds internal state for the drop tracker.
type dropState struct {
	mu            sync.RWMutex
	activeDrops   []ActiveDrop                   // for /api/drops, status=ACTIVE/DISABLED/COMPLETED
	queuedDrops   []ActiveDrop                   // for /api/drops, status=QUEUED
	idleDrops     []ActiveDrop                   // for /api/drops, status=IDLE
	campaignCache map[string]twitch.DropCampaign // campaignID -> campaign, rebuilt each cycle
	currentPickID string                         // ChannelID currently assigned the drop slot, "" if none

	// Stall detection across cycles. After we pick a channel and watch it for one
	// cycle, we check whether Twitch credited any drop minutes. If progress did
	// not increase, the channel is added to stallCooldown for stallCooldownDuration
	// and excluded from the next pool — so we don't get stuck on a channel that
	// Twitch silently refuses to credit.
	lastPickChannelID  string
	lastPickCampaignID string
	lastPickProgress   int
	stallCooldown      map[string]cooldownEntry // channelID → cooldown entry (expiry + reason)

	// lastProgressUpdate tracks when applyDropProgressUpdate last fired
	// (from either WebSocket or poll). pollDropProgressOnce skips its GQL
	// call if a fresh update arrived in the last ~30s — that's TDM's
	// "minute_almost_done" pattern: only query CurrentDrop when our local
	// timer says progress is overdue.
	lastProgressUpdate time.Time

	selector *drops.Selector
}

// dropProgressPollLoop polls Twitch's DropCurrentSessionContext GQL every
// 60 seconds for the currently picked drop channel. This is the bridge that
// keeps progress in sync when user-drop-events PubSub is silent (which is
// most of the time per TwitchDropsMiner research). When a session is found,
// the in-memory ActiveDrops slice is updated via applyDropProgressUpdate.
func (f *Farmer) dropProgressPollLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.pollDropProgressOnce()
		case <-f.stopCh:
			return
		}
	}
}

// pollDropProgressOnce queries DropCurrentSession for the current pick and
// applies the result. Idempotent — safe to call frequently.
//
// Per TDM watch_service.py:205 (minute_almost_done): skip the GQL call if a
// WebSocket update arrived in the last 30s — no point re-fetching what we
// just got pushed. Cuts our DropCurrentSession query rate roughly in half
// when WebSocket is healthy.
func (f *Farmer) pollDropProgressOnce() {
	f.drops.mu.RLock()
	pickedID := f.drops.currentPickID
	pickedCampID := f.drops.lastPickCampaignID
	recentUpdate := !f.drops.lastProgressUpdate.IsZero() && time.Since(f.drops.lastProgressUpdate) < 30*time.Second
	f.drops.mu.RUnlock()

	if pickedID == "" || pickedCampID == "" {
		return
	}
	if recentUpdate {
		return
	}

	session, err := f.gql.GetCurrentDropSession(pickedID)
	if err != nil {
		// Silent — this happens during stream offline/transition; not worth a log.
		return
	}
	if session == nil {
		// Twitch reports no active drop session for this channel. Could mean
		// the streamer's drops aren't crediting us — let stallCooldown handle it.
		return
	}

	f.applyDropProgressUpdate(twitch.DropProgressData{
		CampaignID:             pickedCampID,
		DropID:                 session.DropID,
		CurrentMinutesWatched:  session.CurrentMinutesWatched,
		RequiredMinutesWatched: session.RequiredMinutesWatched,
	})

	// When poll says the current drop is at 100%, do TWO things:
	// 1. Try markCompletedIfFinishedExternally — fetches inventory + only
	//    marks completed if the campaign is genuinely no longer in progress
	//    (i.e., user really finished all drops). For multi-drop campaigns
	//    where one drop is done but more are pending, the campaign WILL
	//    still be in inventory progress, so it stays un-completed.
	// 2. Trigger processDrops so the selector re-evaluates (next drop in
	//    queue gets picked if this one is done, etc).
	if session.RequiredMinutesWatched > 0 && session.CurrentMinutesWatched >= session.RequiredMinutesWatched {
		f.writeLogFile(fmt.Sprintf("[Drops/Poll] drop complete on campaign %s (%d/%d)",
			pickedCampID, session.CurrentMinutesWatched, session.RequiredMinutesWatched))
		go func() {
			f.markCompletedIfFinishedExternally(pickedCampID)
			f.processDrops()
		}()
	}
}

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
	f.autoClaimAndMarkCompleted(campaigns)

	// 2a. Compare the previous pick's drop progress against this cycle's
	//     inventory. If Twitch did not credit any new minutes, put the channel
	//     into stallCooldown so the selector skips it next time.
	f.applyStallDetection(campaigns)

	// 2b. Build the active skip-set from stallCooldown (filtering expired entries).
	skipChannels := f.activeStallSkipSet()

	// 2c. Run the selector on the (now-updated) inventory, with stalled channels skipped.
	pick, pool := f.drops.selector.Select(campaigns, skipChannels)

	// 3. Build per-campaign UI rows.
	active, queued, idle := f.buildDropRows(campaigns, pick, pool)

	// 4. Rebuild campaign cache (for web UI endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 5. Apply pick: register channel as temp if new, set HasActiveDrop.
	pickApplied := f.applySelectorPick(pick, campaigns)

	// 6. If the pick was NOT applied (refresh failed for the new pick), bail
	//    out without committing anything — rows, currentPickID, cleanup, and
	//    snapshot all use the previous pick's state which is still valid.
	//    Next cycle will retry with fresh inventory.
	if !pickApplied {
		f.addLog("[Drops/Pool] skipping commit — pick refresh failed, previous state preserved")
		return
	}

	// 7. Pick applied successfully — commit the new state atomically.
	f.drops.mu.Lock()
	f.drops.activeDrops = active
	f.drops.queuedDrops = queued
	f.drops.idleDrops = idle
	f.drops.campaignCache = newCache
	if pick != nil {
		f.drops.currentPickID = pick.ChannelID
	} else {
		f.drops.currentPickID = ""
	}
	f.drops.mu.Unlock()

	// 8. Drop existing temp channels that are no longer the pick.
	f.cleanupNonPickedTempChannels(pick)

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
	f.snapshotPickProgress(pick, campaigns)
}

// applyStallDetection compares the previous cycle's picked channel against the
// current inventory. If the same drop's CurrentMinutesWatched did not increase,
// Twitch credited zero minutes for our last cycle of watching that channel —
// add it to stallCooldown so we don't pick it again for stallCooldownDuration.
func (f *Farmer) applyStallDetection(campaigns []twitch.DropCampaign) {
	f.drops.mu.Lock()
	defer f.drops.mu.Unlock()

	prevChID := f.drops.lastPickChannelID
	prevCampID := f.drops.lastPickCampaignID
	prevProgress := f.drops.lastPickProgress
	if prevChID == "" || prevCampID == "" {
		return // no previous pick to evaluate
	}

	// Find the previous pick's drop progress in the new inventory.
	currentProgress := -1
	for _, c := range campaigns {
		if c.ID != prevCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			if d.IsClaimed {
				continue
			}
			currentProgress = d.CurrentMinutesWatched
			break
		}
		break
	}

	if currentProgress < 0 {
		// Campaign disappeared from inventory or fully claimed. Either way,
		// no stall to record.
		return
	}

	if currentProgress > prevProgress {
		// Twitch credited at least one minute — channel is healthy.
		// Clear ONLY a stall-reason cooldown. Manual cooldowns (game-change,
		// id-mismatch) must run their own timer so user-deliberate skips
		// aren't undone by a single credited minute.
		if cd, ok := f.drops.stallCooldown[prevChID]; ok && cd.reason == cooldownStall {
			delete(f.drops.stallCooldown, prevChID)
		}
		return
	}

	// No credit since last cycle — record a stall-reason cooldown.
	if f.drops.stallCooldown == nil {
		f.drops.stallCooldown = make(map[string]cooldownEntry)
	}
	f.drops.stallCooldown[prevChID] = cooldownEntry{
		expires: time.Now().Add(stallCooldownDuration),
		reason:  cooldownStall,
	}
	f.addLog("[Drops/Pool] no credit on %s (progress stuck at %d/%d) — %v cooldown",
		prevChID, currentProgress, prevProgress, stallCooldownDuration)
}

// setManualCooldown adds a manual-reason cooldown for the given channel.
// Manual cooldowns are NOT cleared by applyStallDetection on credit — only
// the timeout removes them. Used by handleChannelGameChange and the
// applySelectorPick guards (wrong game, id mismatch).
func (f *Farmer) setManualCooldown(channelID string, dur time.Duration) {
	f.drops.mu.Lock()
	defer f.drops.mu.Unlock()
	if f.drops.stallCooldown == nil {
		f.drops.stallCooldown = make(map[string]cooldownEntry)
	}
	f.drops.stallCooldown[channelID] = cooldownEntry{
		expires: time.Now().Add(dur),
		reason:  cooldownManual,
	}
}

// activeStallSkipSet returns channelIDs currently in cooldown, expiring entries
// removed from the underlying map as a side effect.
func (f *Farmer) activeStallSkipSet() map[string]bool {
	f.drops.mu.Lock()
	defer f.drops.mu.Unlock()

	skip := make(map[string]bool, len(f.drops.stallCooldown))
	now := time.Now()
	for chID, cd := range f.drops.stallCooldown {
		if now.Before(cd.expires) {
			skip[chID] = true
		} else {
			delete(f.drops.stallCooldown, chID)
		}
	}
	return skip
}

// snapshotPickProgress records the picked channel's primary-campaign drop
// progress so the next cycle's applyStallDetection can compare.
func (f *Farmer) snapshotPickProgress(pick *drops.PoolEntry, campaigns []twitch.DropCampaign) {
	f.drops.mu.Lock()
	defer f.drops.mu.Unlock()

	if pick == nil || len(pick.Campaigns) == 0 {
		f.drops.lastPickChannelID = ""
		f.drops.lastPickCampaignID = ""
		f.drops.lastPickProgress = 0
		return
	}

	primaryCampID := pick.Campaigns[0].ID
	progress := 0
	for _, c := range campaigns {
		if c.ID != primaryCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
				continue
			}
			progress = d.CurrentMinutesWatched
			break
		}
		break
	}
	f.drops.lastPickChannelID = pick.ChannelID
	f.drops.lastPickCampaignID = primaryCampID
	f.drops.lastPickProgress = progress
}

// autoClaimAndMarkCompleted handles drop claims and marks fully-claimed
// campaigns as completed in config.
func (f *Farmer) autoClaimAndMarkCompleted(campaigns []twitch.DropCampaign) {
	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if f.cfg.IsCampaignCompleted(c.ID) {
			continue
		}

		allClaimed := true
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			hasWatchable = true
			if d.IsClaimed {
				continue
			}
			allClaimed = false
			if d.IsComplete() && d.DropInstanceID != "" {
				name := d.BenefitName
				if name == "" {
					name = d.Name
				}
				instanceID := d.DropInstanceID
				dropName := name
				campaignName := c.Name
				go func() {
					if err := f.gql.ClaimDrop(instanceID); err != nil {
						f.addLog("[Drops] Failed to claim %s: %v", dropName, err)
					} else {
						f.addLog("[Drops] Claimed: %s (%s)", dropName, campaignName)
					}
				}()
			}
		}

		if hasWatchable && allClaimed {
			f.cfg.MarkCampaignCompleted(c.ID)
			f.cfg.Save()
			f.addLog("[Drops] Campaign %q fully claimed — marked as completed", c.Name)
		}
		// NOTE: don't auto-mark on !c.InInventory alone — that signal is
		// ambiguous (could be "user finished externally" OR "user never started").
		// Instead pollDropProgressOnce handles "finished externally" by polling
		// DropCurrentSession on the picked channel — when the channel IS picked
		// AND poll says current >= required AND inventory shows !InInventory,
		// THAT combination is reliable. See markCompletedIfFinishedExternally.
	}
}

// handleDropClaim is the sequential, TDM-aligned drop-claim flow. It:
//  1. Claims the drop (synchronous — must succeed before we re-evaluate state)
//  2. Sleeps 4s (Twitch's backend takes a moment to advance the drop session)
//  3. Polls DropCurrentSession up to 8× (with 2s sleep) waiting for the
//     dropID to change — i.e. Twitch has advanced to the next drop in the
//     campaign or the campaign is now done
//  4. Triggers processDrops to re-pick / mark completed
//
// This sequencing prevents the v1.8.0 race where parallel claim + processDrops
// goroutines saw stale unclaimed state.
func (f *Farmer) handleDropClaim(data twitch.DropClaimData) {
	if data.DropInstanceID != "" {
		if err := f.gql.ClaimDrop(data.DropInstanceID); err != nil {
			f.addLog("[Drops/WS] Failed to claim drop: %v", err)
		} else {
			f.addLog("[Drops/WS] Claimed drop instance %s", data.DropInstanceID)
		}
	}

	// Wait for Twitch to advance the session.
	time.Sleep(4 * time.Second)

	f.drops.mu.RLock()
	pickedID := f.drops.currentPickID
	f.drops.mu.RUnlock()

	if pickedID != "" && data.DropID != "" {
		for i := 0; i < 8; i++ {
			session, err := f.gql.GetCurrentDropSession(pickedID)
			if err != nil {
				break
			}
			if session == nil || session.DropID != data.DropID {
				// Either no more session for this channel (campaign done)
				// or session has advanced to the next drop.
				break
			}
			time.Sleep(2 * time.Second)
		}
	}

	f.processDrops()
}

// markCompletedIfFinishedExternally is called by pollDropProgressOnce when the
// poll says the picked channel's drop is at 100%. It re-fetches inventory and
// confirms the campaign is no longer in dropCampaignsInProgress before marking
// completed. This combo is reliable: poll says "Twitch credited me past
// required" + inventory says "campaign no longer in progress list" =
// genuinely done. Avoids the false-positive of marking never-started
// campaigns as completed just because they aren't in the inventory yet.
func (f *Farmer) markCompletedIfFinishedExternally(campaignID string) {
	campaigns, err := f.gql.GetDropsInventory()
	if err != nil {
		return
	}
	for _, c := range campaigns {
		if c.ID != campaignID {
			continue
		}
		if c.InInventory {
			// Still in progress — has more drops to farm. Don't mark.
			return
		}
		if f.cfg.IsCampaignCompleted(c.ID) {
			return
		}
		f.cfg.MarkCampaignCompleted(c.ID)
		_ = f.cfg.Save()
		f.addLog("[Drops] Campaign %q finished externally (poll: complete + not in inventory) — marked completed", c.Name)
		return
	}
}

// buildDropRows produces the per-campaign UI rows for the web API.
func (f *Farmer) buildDropRows(
	campaigns []twitch.DropCampaign,
	pick *drops.PoolEntry,
	pool []*drops.PoolEntry,
) (active, queued, idle []ActiveDrop) {
	pinnedID := f.cfg.GetPinnedCampaign()

	campaignsInPool := make(map[string]*drops.PoolEntry)
	for _, e := range pool {
		for _, ref := range e.Campaigns {
			if _, exists := campaignsInPool[ref.ID]; !exists {
				campaignsInPool[ref.ID] = e
			}
		}
	}

	pickedCampaignIDs := make(map[string]bool)
	if pick != nil {
		for _, ref := range pick.Campaigns {
			pickedCampaignIDs[ref.ID] = true
		}
	}

	queueIdx := 1
	seenWatchableNames := make(map[string]bool) // dedup sub-only-deduped campaign noise (e.g. 9× "S5 Support ABI Partners")
	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(time.Now()) {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}

		// Skip campaigns with no watchable drops (sub-only, or all drops claimed).
		// These can't be farmed, so showing them in the queue is just noise.
		// EXCEPTION: keep them if disabled or completed so the user can see why.
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasWatchable = true
				break
			}
		}
		if !hasWatchable && !f.cfg.IsCampaignDisabled(c.ID) && !f.cfg.IsCampaignCompleted(c.ID) {
			continue
		}

		// Dedup by name: when Twitch returns N copies of the same campaign with
		// different IDs (each with one allowed channel — typical for streamer-
		// exclusive drops), show only the first. The selector still considers
		// all of them; this is purely a UI dedup.
		if seenWatchableNames[c.Name] {
			continue
		}
		seenWatchableNames[c.Name] = true

		row := campaignToRow(c, pinnedID)

		switch {
		case f.cfg.IsCampaignDisabled(c.ID):
			row.Status = "DISABLED"
			active = append(active, row)
		case f.cfg.IsCampaignCompleted(c.ID):
			row.Status = "COMPLETED"
			active = append(active, row)
		case pickedCampaignIDs[c.ID]:
			row.Status = "ACTIVE"
			row.QueueIndex = queueIdx
			queueIdx++
			if pick != nil {
				row.ChannelLogin = pick.ChannelLogin
			}
			active = append(active, row)
		case campaignsInPool[c.ID] != nil:
			row.Status = "QUEUED"
			row.QueueIndex = queueIdx
			queueIdx++
			queued = append(queued, row)
		default:
			row.Status = "IDLE"
			idle = append(idle, row)
		}
	}

	return active, queued, idle
}

// campaignToRow projects a DropCampaign into the ActiveDrop UI shape.
func campaignToRow(c twitch.DropCampaign, pinnedID string) ActiveDrop {
	var dropName string
	var progress, required int
	for _, d := range c.Drops {
		if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
			continue
		}
		dropName = d.BenefitName
		if dropName == "" {
			dropName = d.Name
		}
		progress = d.CurrentMinutesWatched
		required = d.RequiredMinutesWatched
		break
	}

	pct := 0
	if required > 0 {
		pct = (progress * 100) / required
		if pct > 100 {
			pct = 100
		}
	}

	eta := required - progress
	if eta < 0 {
		eta = 0
	}

	return ActiveDrop{
		CampaignID:         c.ID,
		CampaignName:       c.Name,
		GameName:           c.GameName,
		DropName:           dropName,
		Progress:           progress,
		Required:           required,
		Percent:            pct,
		EndAt:              c.EndAt,
		IsEnabled:          true,
		IsAccountConnected: c.IsAccountConnected,
		IsPinned:           c.ID == pinnedID && pinnedID != "",
		EtaMinutes:         eta,
	}
}

// applySelectorPick registers the picked channel as a temp channel if not
// already tracked, sets HasActiveDrop=true on it, and clears HasActiveDrop on
// any other channel that was the previous pick.
//
// Returns true on success (state mutated, Watcher started OR pick==nil cleared
// state cleanly). Returns false when the metadata refresh failed for a non-nil
// pick — callers must NOT commit currentPickID in that case, otherwise we
// end up with state believing "drop is running" while no Watcher is active.
func (f *Farmer) applySelectorPick(pick *drops.PoolEntry, campaigns []twitch.DropCampaign) bool {
	f.drops.mu.RLock()
	prevPickID := f.drops.currentPickID
	f.drops.mu.RUnlock()

	if pick == nil {
		if prevPickID != "" {
			if ch, ok := f.channels.Get(prevPickID); ok {
				ch.ClearDropInfo()
				// Clear IsWatching so rotation can pick this channel up
				// again as a normal Spade slot.
				ch.SetWatching(false)
			}
			f.unsubscribeBroadcastSettings(prevPickID)
		}
		if f.dropWatch != nil {
			f.dropWatch.Stop()
		}
		return true
	}

	// 1. SINGLE source of truth for metadata: fetch upfront BEFORE any state
	//    mutation. If this fails, NO channel is added, NO drop info changes,
	//    NO previous pick is released, NO topics are subscribed. The next
	//    cycle retries cleanly with the existing pick still in effect.
	info, err := f.gql.GetChannelInfo(pick.ChannelLogin)
	if err != nil || info == nil || !info.IsLive || info.BroadcastID == "" || info.GameID == "" {
		liveStr := "false"
		bidStr := ""
		gidStr := ""
		if info != nil {
			if info.IsLive {
				liveStr = "true"
			}
			bidStr = info.BroadcastID
			gidStr = info.GameID
		}
		f.addLog("[Drops/Watch] skip %s — refresh failed (live=%s broadcast=%q game_id=%q)",
			pick.ChannelLogin, liveStr, bidStr, gidStr)
		return false
	}

	// 2. Game-match guard: streamer may have switched games between selector
	//    run and now. If the freshly-fetched game doesn't match any of the
	//    pick's campaigns, abort — sending sendSpadeEvents with a wrong
	//    game_id makes Twitch silently drop credit.
	if !drops.PickGameMatches(pick, info.GameName) {
		f.addLog("[Drops/Watch] skip %s — game changed to %q (expected one of %s)",
			pick.ChannelLogin, info.GameName, drops.PickCampaignGames(pick))
		// Manual-reason cooldown so the selector doesn't immediately re-pick.
		f.setManualCooldown(pick.ChannelID, 15*time.Minute)
		return false
	}

	// 3. Channel-ID consistency: pick.ChannelID came from the selector pool
	//    (built from directory or allowed_channels). info.ID came from a
	//    direct user(login:) lookup just now. They MUST match — if they
	//    don't, our internal channels[] map (keyed by ChannelID) will get
	//    confused (e.g., create a temp with info.ID but later look it up
	//    with pick.ChannelID and miss it, leaving an orphaned temp).
	if info.ID != pick.ChannelID {
		f.addLog("[Drops/Watch] skip %s — id mismatch (pick=%s info=%s) — cooldown",
			pick.ChannelLogin, pick.ChannelID, info.ID)
		// Cooldown the broken pool ID so the selector doesn't immediately
		// re-pick the same wrong entry next cycle.
		f.setManualCooldown(pick.ChannelID, 30*time.Minute)
		return false
	}

	// 4. Resolve or create channel state, using the already-fetched info.
	//    No second GetChannelInfo call — same data drives temp creation
	//    AND watcher start, so we can't end up with a registered temp that
	//    failed its refresh.
	ch, exists := f.channels.Get(pick.ChannelID)
	if !exists {
		primaryCampID := ""
		if len(pick.Campaigns) > 0 {
			primaryCampID = pick.Campaigns[0].ID
		}
		if err := f.addTemporaryChannelFromInfo(info, primaryCampID); err != nil {
			f.addLog("[Drops/Pool] failed to add %s: %v", pick.ChannelLogin, err)
			return false
		}
		ch, exists = f.channels.Get(pick.ChannelID)
		if !exists {
			return false
		}
	} else {
		// Existing channel — refresh its state with the verified metadata.
		ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
	}
	snap := ch.Snapshot()

	// 3. Metadata is valid — NOW it's safe to mutate state.
	primaryCampID := ""
	if len(pick.Campaigns) > 0 {
		primaryCampID = pick.Campaigns[0].ID
	}
	for _, c := range campaigns {
		if c.ID != primaryCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
				continue
			}
			name := d.BenefitName
			if name == "" {
				name = d.Name
			}
			ch.SetDropInfo(name, d.CurrentMinutesWatched, d.RequiredMinutesWatched)
			ch.SetCampaignID(c.ID)
			break
		}
		break
	}

	// 4. Release previous pick (only if it's a different channel).
	if prevPickID != "" && prevPickID != pick.ChannelID {
		if prevCh, ok := f.channels.Get(prevPickID); ok {
			prevCh.ClearDropInfo()
			prevCh.SetWatching(false)
		}
		f.unsubscribeBroadcastSettings(prevPickID)
	}

	// 5. Subscribe broadcast-settings-update for the new pick.
	f.subscribeBroadcastSettings(pick.ChannelID)

	// 6. Hand the channel to the drops Watcher.
	if f.dropWatch != nil {
		f.spade.StopWatching(snap.ChannelID)
		f.prober.Stop(snap.Login)
		ch.SetWatching(true) // for UI display
		f.dropWatch.Start(snap.ChannelID, snap.Login, snap.BroadcastID, snap.GameName, snap.GameID)
		f.addLog("[Drops/Watch] handing %s to drops Watcher (exclusive)", snap.DisplayName)
	}
	return true
}

// subscribeBroadcastSettings subscribes to broadcast-settings-update for one channel.
func (f *Farmer) subscribeBroadcastSettings(channelID string) {
	if f.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := f.pubsub.Listen([]string{topic}); err != nil {
		f.addLog("[PubSub] subscribe %s failed: %v", topic, err)
	}
}

// unsubscribeBroadcastSettings drops the broadcast-settings-update topic for one channel.
func (f *Farmer) unsubscribeBroadcastSettings(channelID string) {
	if f.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := f.pubsub.Unlisten([]string{topic}); err != nil {
		f.addLog("[PubSub] unsubscribe %s failed: %v", topic, err)
	}
}

// cleanupNonPickedTempChannels removes every temporary channel that is NOT
// the current pick.
func (f *Farmer) cleanupNonPickedTempChannels(pick *drops.PoolEntry) {
	pickID := ""
	if pick != nil {
		pickID = pick.ChannelID
	}

	var stale []string
	for _, ch := range f.channels.States() {
		snap := ch.Snapshot()
		if snap.IsTemporary && snap.ChannelID != pickID {
			stale = append(stale, snap.ChannelID)
		}
	}

	for _, chID := range stale {
		f.removeTemporaryChannel(chID)
	}
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
func (f *Farmer) GetActiveDrops() []ActiveDrop {
	f.drops.mu.RLock()
	defer f.drops.mu.RUnlock()

	total := len(f.drops.activeDrops) + len(f.drops.queuedDrops) + len(f.drops.idleDrops)
	if total == 0 {
		return nil
	}
	out := make([]ActiveDrop, 0, total)
	out = append(out, f.drops.activeDrops...)
	out = append(out, f.drops.queuedDrops...)
	out = append(out, f.drops.idleDrops...)
	return out
}

// GetEligibleGames returns the unique sorted list of game names from the
// current cycle's inventory (account's currently-known campaigns). Used as
// the default autocomplete pool. For free-text search across ALL Twitch
// categories (e.g. "tarkov" before any EFT campaign appears in inventory),
// callers should additionally hit /api/games/search backed by SearchGameCategories.
func (f *Farmer) GetEligibleGames() []string {
	f.drops.mu.RLock()
	defer f.drops.mu.RUnlock()

	seen := make(map[string]bool)
	var out []string
	for _, c := range f.drops.campaignCache {
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

// applyDropProgressUpdate handles a WebSocket drop-progress event by updating
// the in-memory ActiveDrops slice (so the web/TUI sees the new value within
// 1 second instead of waiting for the next 15-min poll). Also updates the
// matching channel's HasActiveDrop progress for TUI rendering.
//
// Per-spec section 6.6: matches by (CampaignID, DropID). When the payload's
// DropID differs from the cached ActiveDrop's drop, the cached row's
// DropName + Required + Progress are all replaced from the payload (via a
// lookup against campaignCache to find the human-readable drop name).
//
// Per TDM message_handlers.py:251 (drop.can_earn before update_minutes):
// skip the update entirely if the campaign is currently disabled or completed
// — the payload is for a drop we shouldn't be earning, displaying it would
// mislead the user.
func (f *Farmer) applyDropProgressUpdate(data twitch.DropProgressData) {
	// can_earn equivalent: skip if campaign isn't currently farmable.
	if f.cfg.IsCampaignDisabled(data.CampaignID) || f.cfg.IsCampaignCompleted(data.CampaignID) {
		return
	}
	// Resolve the payload's drop name from the campaign cache so the UI can
	// show "DROP 6" instead of stale "DROP 1" when Twitch's session has
	// advanced to a later drop in a multi-drop campaign.
	resolvedName := ""
	resolvedRequired := data.RequiredMinutesWatched
	f.drops.mu.RLock()
	if c, ok := f.drops.campaignCache[data.CampaignID]; ok {
		for _, d := range c.Drops {
			if d.ID == data.DropID {
				resolvedName = d.BenefitName
				if resolvedName == "" {
					resolvedName = d.Name
				}
				if resolvedRequired == 0 && d.RequiredMinutesWatched > 0 {
					resolvedRequired = d.RequiredMinutesWatched
				}
				break
			}
		}
	}
	f.drops.mu.RUnlock()

	f.drops.mu.Lock()
	updated := false
	for i := range f.drops.activeDrops {
		if f.drops.activeDrops[i].CampaignID != data.CampaignID {
			continue
		}
		// If the cached row already targets a different drop, swap to the new one.
		if data.DropID != "" && resolvedName != "" {
			f.drops.activeDrops[i].DropName = resolvedName
		}
		if resolvedRequired > 0 {
			f.drops.activeDrops[i].Required = resolvedRequired
		}
		f.drops.activeDrops[i].Progress = data.CurrentMinutesWatched
		if f.drops.activeDrops[i].Required > 0 {
			pct := (data.CurrentMinutesWatched * 100) / f.drops.activeDrops[i].Required
			if pct > 100 {
				pct = 100
			}
			f.drops.activeDrops[i].Percent = pct
			f.drops.activeDrops[i].EtaMinutes = f.drops.activeDrops[i].Required - data.CurrentMinutesWatched
			if f.drops.activeDrops[i].EtaMinutes < 0 {
				f.drops.activeDrops[i].EtaMinutes = 0
			}
		}
		updated = true
		break
	}
	// IMPORTANT: do NOT mutate lastPickProgress here. That field is the
	// stall-detection baseline and must come exclusively from snapshotPickProgress
	// at the end of each inventory cycle. Live WebSocket / poll updates
	// rewriting it would shift the baseline forward between cycles, causing
	// healthy channels to register as stalled at the next applyStallDetection.

	// Mark the timestamp so pollDropProgressOnce can skip its GQL call if a
	// fresh WS event already updated the same data (TDM minute_almost_done).
	if updated {
		f.drops.lastProgressUpdate = time.Now()
	}
	f.drops.mu.Unlock()

	if updated {
		f.writeLogFile(fmt.Sprintf("[Drops/WS] progress: campaign=%s drop=%s %d minutes",
			data.CampaignID, data.DropID, data.CurrentMinutesWatched))

		// Mirror to picked channel's drop info so TUI shows the live value.
		// FIX: when the campaign advances to a new drop (e.g., drop 1 done →
		// drop 2 starts), the resolved name/required from the payload differ
		// from the channel's previously-stored snap.DropName/snap.DropRequired.
		// Use the resolved values, not the stale snap values, so the TUI/
		// channel view stays in sync with the activeDrops table.
		nextName := resolvedName
		nextRequired := resolvedRequired
		f.drops.mu.RLock()
		pickedCh := f.drops.currentPickID
		f.drops.mu.RUnlock()
		if pickedCh != "" {
			if ch, ok := f.channels.Get(pickedCh); ok {
				snap := ch.Snapshot()
				if snap.HasActiveDrop {
					if nextName == "" {
						nextName = snap.DropName // fall back if cache miss
					}
					if nextRequired <= 0 {
						nextRequired = snap.DropRequired
					}
					ch.SetDropInfo(nextName, data.CurrentMinutesWatched, nextRequired)
				}
			}
		}
	}
}

// handleChannelGameChange reacts to a broadcast-settings-update PubSub event.
// If the channel was the current drop pick AND the new game does not match
// the picked campaign's game, the channel is added to stallCooldown for
// 15 min and an out-of-cycle selector re-run is triggered.
//
// Per TDM message_handlers.py:121 (check_online → ONLINE_DELAY 120s): we
// debounce 30s before reacting. Streamers often flap game/title rapidly
// (especially during stream-start or category transitions), and reacting
// instantly causes unnecessary channel switches. After 30s, re-fetch the
// channel's actual current game; if the streamer has switched back to the
// expected game by then, no action.
func (f *Farmer) handleChannelGameChange(channelID string, data twitch.GameChangeData) {
	f.drops.mu.RLock()
	currentPick := f.drops.currentPickID
	pickCampaign := f.drops.lastPickCampaignID
	f.drops.mu.RUnlock()

	if channelID != currentPick {
		if data.OldGameName != data.NewGameName {
			f.writeLogFile(fmt.Sprintf("[Drops/WS] non-pick channel %s game changed: %s -> %s",
				channelID, data.OldGameName, data.NewGameName))
		}
		return
	}

	f.drops.mu.RLock()
	expectedGame := ""
	pickedChannelLogin := ""
	if c, ok := f.drops.campaignCache[pickCampaign]; ok {
		expectedGame = c.GameName
	}
	if ch, ok := f.channels.Get(channelID); ok {
		pickedChannelLogin = ch.Login
	}
	f.drops.mu.RUnlock()

	if expectedGame == "" || pickedChannelLogin == "" {
		return
	}

	// FIX: same-game broadcast-settings-update events ALSO need to refresh
	// the Watcher — the streamer may have restarted the broadcast (new
	// broadcast_id with same game) or changed title/tags. Without this
	// refresh, the Watcher keeps sending the old broadcast_id and Twitch
	// silently drops credit until the next pick cycle.
	if data.OldGameName == data.NewGameName {
		go f.refreshWatcherBroadcast(channelID, pickedChannelLogin)
		return
	}

	// Optimistic early-out: payload already shows we're back on the right game.
	if strings.EqualFold(data.NewGameName, expectedGame) {
		f.addLog("[Drops/WS] %s switched back to %q — keeping pick", channelID, expectedGame)
		go f.refreshWatcherBroadcast(channelID, pickedChannelLogin)
		return
	}

	// Debounce 30s, then re-verify via fresh GetChannelInfo before applying
	// the cooldown. Absorbs streamer flapping.
	go func() {
		time.Sleep(30 * time.Second)

		// Re-check whether this is still the picked channel (selector may have
		// moved on while we slept).
		f.drops.mu.RLock()
		stillPicked := f.drops.currentPickID == channelID
		f.drops.mu.RUnlock()
		if !stillPicked {
			return
		}

		info, err := f.gql.GetChannelInfo(pickedChannelLogin)
		if err == nil && strings.EqualFold(info.GameName, expectedGame) {
			f.addLog("[Drops/WS] %s flapped back to %q during 30s debounce — keeping pick",
				channelID, expectedGame)
			return
		}

		f.setManualCooldown(channelID, 15*time.Minute)

		f.addLog("[Drops/WS] %s changed game (%s -> %s); still wrong after 30s — 15min cooldown, re-picking",
			channelID, data.OldGameName, data.NewGameName)

		f.processDrops()
	}()
}

// refreshWatcherBroadcast fetches the channel's current stream metadata and
// pushes it into the running drops Watcher. Used when broadcast-settings-update
// fires for the currently-picked channel and the streamer is still on the
// expected game — Twitch may have issued a new broadcast_id mid-session
// (stream restart, title change, etc.) and the Watcher must use the new one
// in subsequent sendSpadeEvents heartbeats.
func (f *Farmer) refreshWatcherBroadcast(channelID, login string) {
	if f.dropWatch == nil {
		return
	}
	info, err := f.gql.GetChannelInfo(login)
	if err != nil || info == nil || !info.IsLive {
		return
	}
	// FIX: don't push empty IDs into the Watcher. GetChannelInfo can
	// momentarily return IsLive=true with an empty broadcast_id during a
	// stream-restart transition; the Watcher would then send heartbeats
	// with broadcast_id="" until the next refresh.
	if info.BroadcastID == "" || info.GameID == "" {
		return
	}
	if ch, ok := f.channels.Get(channelID); ok {
		ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
	}
	f.dropWatch.UpdateBroadcast(channelID, info.BroadcastID, info.GameName, info.GameID)
}

// (scrubStaleCompleted was removed — it conflicted with the
// DropCurrentSession poll-based completion which is now authoritative.
// Daily-rolling campaigns where Twitch reuses the same ID across days
// will need manual un-disable via TUI 't' if they get stuck completed —
// in practice Twitch tends to issue new campaign IDs per day.)
