package farmer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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

// dropState holds internal state for the drop tracker.
type dropState struct {
	mu            sync.RWMutex
	activeDrops   []ActiveDrop                   // for /api/drops, status=ACTIVE/DISABLED/COMPLETED
	queuedDrops   []ActiveDrop                   // for /api/drops, status=QUEUED
	idleDrops     []ActiveDrop                   // for /api/drops, status=IDLE
	campaignCache map[string]twitch.DropCampaign // campaignID -> campaign, rebuilt each cycle
	currentPickID string                         // ChannelID currently assigned the drop slot, "" if none

	selector *DropSelector
}

// dropCheckLoop polls the drops inventory every 10 minutes.
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

	ticker := time.NewTicker(5 * time.Minute)
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

	// 1. Auto-claim any drops that are complete and have an instance ID.
	f.autoClaimAndMarkCompleted(campaigns)

	// 2. Run the selector on the (now-updated) inventory.
	pick, pool := f.drops.selector.Select(campaigns)

	// 3. Build per-campaign UI rows.
	active, queued, idle := f.buildDropRows(campaigns, pick, pool)

	// 4. Rebuild campaign cache (for web UI endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 5. Apply pick: register channel as temp if new, set HasActiveDrop.
	f.applySelectorPick(pick, campaigns)

	// 6. Store rows + cache atomically.
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

	// 7. Drop existing temp channels that are no longer the pick.
	f.cleanupNonPickedTempChannels(pick)

	// 8. Trigger rotation so Spade slot 1 reflects the new pick.
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
	}
}

// buildDropRows produces the per-campaign UI rows for the web API.
func (f *Farmer) buildDropRows(
	campaigns []twitch.DropCampaign,
	pick *PoolEntry,
	pool []*PoolEntry,
) (active, queued, idle []ActiveDrop) {
	pinnedID := f.cfg.GetPinnedCampaign()

	campaignsInPool := make(map[string]*PoolEntry)
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
func (f *Farmer) applySelectorPick(pick *PoolEntry, campaigns []twitch.DropCampaign) {
	f.drops.mu.RLock()
	prevPickID := f.drops.currentPickID
	f.drops.mu.RUnlock()

	if pick == nil {
		if prevPickID != "" {
			f.mu.RLock()
			ch, ok := f.channels[prevPickID]
			f.mu.RUnlock()
			if ok {
				ch.ClearDropInfo()
			}
		}
		return
	}

	f.mu.RLock()
	ch, exists := f.channels[pick.ChannelID]
	f.mu.RUnlock()

	if !exists {
		primaryCampID := ""
		if len(pick.Campaigns) > 0 {
			primaryCampID = pick.Campaigns[0].ID
		}
		if err := f.addTemporaryChannel(pick.ChannelLogin, primaryCampID); err != nil {
			f.addLog("[Drops/Pool] failed to add %s: %v", pick.ChannelLogin, err)
			return
		}
		f.mu.RLock()
		ch = f.channels[pick.ChannelID]
		f.mu.RUnlock()
		if ch == nil {
			return
		}
	}

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
			ch.mu.Lock()
			ch.CampaignID = c.ID
			ch.mu.Unlock()
			break
		}
		break
	}

	if prevPickID != "" && prevPickID != pick.ChannelID {
		f.mu.RLock()
		prevCh, ok := f.channels[prevPickID]
		f.mu.RUnlock()
		if ok {
			prevCh.ClearDropInfo()
		}
	}
}

// cleanupNonPickedTempChannels removes every temporary channel that is NOT
// the current pick.
func (f *Farmer) cleanupNonPickedTempChannels(pick *PoolEntry) {
	pickID := ""
	if pick != nil {
		pickID = pick.ChannelID
	}

	f.mu.RLock()
	var stale []string
	for chID, ch := range f.channels {
		if ch.Snapshot().IsTemporary && chID != pickID {
			stale = append(stale, chID)
		}
	}
	f.mu.RUnlock()

	for _, chID := range stale {
		f.removeTemporaryChannel(chID)
	}
}

// cleanupTemporaryChannels removes temp channels that are no longer useful.
// Only temp channels in activeDropChannels (assigned a drop this cycle) are kept.
func (f *Farmer) cleanupTemporaryChannels(activeDropChannels map[string]bool) {
	f.mu.RLock()
	var staleChannels []string
	for chID, ch := range f.channels {
		snap := ch.Snapshot()
		if !snap.IsTemporary {
			continue
		}
		// Keep temp channels that are actively assigned to a drop this cycle
		if activeDropChannels[chID] {
			continue
		}
		// Everything else is stale — campaign ended, exclusive-filtered, replaced by failover, etc.
		staleChannels = append(staleChannels, chID)
	}
	f.mu.RUnlock()

	for _, chID := range staleChannels {
		f.removeTemporaryChannel(chID)
	}
}

// checkDropProgressStalls compares this cycle's drop progress against the
// previous cycle for every temp channel that is currently assigned to a drop.
// If the same temp channel + campaign combination appears in two consecutive
// cycles AND the watch-minute progress did not increase, Twitch is not
// crediting our viewing — usually because the stream is not drops-enabled
// (the directory filter may have missed it, or the streamer toggled drops off
// or switched game after we picked them). After stallThreshold consecutive
// stuck cycles we trigger handleDropFailover so we don't burn hours on a dead
// channel like we did with samoylov___ on 2026-04-28.
func (f *Farmer) checkDropProgressStalls(oldDrops, newDrops []ActiveDrop) {
	const stallThreshold = 2 // ≈ 10 min stuck (5-min cycle) → fail over

	type key struct{ chID, campID string }

	// Snapshot previous progress per channel+campaign.
	oldProgress := make(map[key]int, len(oldDrops))
	for _, ad := range oldDrops {
		f.mu.RLock()
		chID := f.loginMap[ad.ChannelLogin]
		f.mu.RUnlock()
		if chID == "" {
			continue
		}
		oldProgress[key{chID, ad.CampaignID}] = ad.Progress
	}

	type victim struct {
		chID  string
		login string
	}
	var victims []victim

	f.drops.mu.Lock()
	if f.drops.dropStallCount == nil {
		f.drops.dropStallCount = make(map[string]int)
	}
	newStallCount := make(map[string]int)
	for _, ad := range newDrops {
		f.mu.RLock()
		chID := f.loginMap[ad.ChannelLogin]
		isTemp := false
		if chID != "" {
			if ch, ok := f.channels[chID]; ok {
				isTemp = ch.Snapshot().IsTemporary
			}
		}
		f.mu.RUnlock()
		if chID == "" || !isTemp {
			continue
		}
		prev, hadPrev := oldProgress[key{chID, ad.CampaignID}]
		if !hadPrev || prev != ad.Progress {
			// First cycle on this assignment, or progress moved → not stalled, reset.
			continue
		}
		count := f.drops.dropStallCount[chID] + 1
		if count >= stallThreshold {
			victims = append(victims, victim{chID, ad.ChannelLogin})
			// Don't carry the count forward — failover effectively resets it.
			continue
		}
		newStallCount[chID] = count
	}
	f.drops.dropStallCount = newStallCount
	f.drops.mu.Unlock()

	for _, v := range victims {
		f.addLog("[Drops/Health] Temp channel %s assigned but no drop progress for %d cycles — likely not drops-enabled, triggering failover", v.login, stallThreshold)
		f.handleDropFailover(v.chID)
	}
}

// cleanupFailoverCooldowns removes cooldown entries older than 30 minutes.
func (f *Farmer) cleanupFailoverCooldowns() {
	f.drops.mu.Lock()
	defer f.drops.mu.Unlock()

	if f.drops.failoverCooldowns == nil {
		return
	}
	for id, t := range f.drops.failoverCooldowns {
		if time.Since(t) > 30*time.Minute {
			delete(f.drops.failoverCooldowns, id)
		}
	}
}

// matchCampaignChannels returns a map of channelID -> login for configured channels
// that are eligible for the given campaign.
func (f *Farmer) matchCampaignChannels(campaign twitch.DropCampaign) map[string]string {
	result := make(map[string]string)

	f.mu.RLock()
	defer f.mu.RUnlock()

	if len(campaign.Channels) > 0 {
		// Campaign specifies allowed channels — match against our configured channels
		for _, dropCh := range campaign.Channels {
			login := strings.ToLower(dropCh.Name)
			if chID, ok := f.loginMap[login]; ok {
				result[chID] = login
			}
			// Also try matching by channel ID directly
			if _, ok := f.channels[dropCh.ID]; ok {
				ch := f.channels[dropCh.ID]
				result[dropCh.ID] = ch.Login
			}
		}
	} else {
		// No channel restrictions — match by game name against online channels
		for chID, ch := range f.channels {
			snap := ch.Snapshot()
			if snap.GameName != "" && strings.EqualFold(snap.GameName, campaign.GameName) {
				result[chID] = ch.Login
			}
		}
	}

	return result
}

// spadeSlotsSaturated returns true if 2+ existing online P0 channels already have
// campaign EndAt earlier than the given campaign. In that case rotateChannels would
// never give a new temp channel a Spade slot (P0 sorts by EndAt, only top 2 watch),
// so creating one would just leave a dead channel sitting around.
func (f *Farmer) spadeSlotsSaturated(campaign twitch.DropCampaign) bool {
	const maxSlots = 2

	var p0CampaignIDs []string
	f.mu.RLock()
	for _, ch := range f.channels {
		snap := ch.Snapshot()
		if snap.IsOnline && snap.HasActiveDrop && snap.CampaignID != "" {
			p0CampaignIDs = append(p0CampaignIDs, snap.CampaignID)
		}
	}
	f.mu.RUnlock()

	if len(p0CampaignIDs) < maxSlots {
		return false
	}

	earlierCount := 0
	f.drops.mu.RLock()
	for _, cid := range p0CampaignIDs {
		c, ok := f.drops.campaignCache[cid]
		if !ok || c.EndAt.IsZero() {
			// Zero EndAt sorts LAST in rotateChannels, so it would lose to us — don't count.
			continue
		}
		if campaign.EndAt.IsZero() {
			// We have zero EndAt → any concrete EndAt beats us in the sort.
			earlierCount++
		} else if c.EndAt.Before(campaign.EndAt) {
			earlierCount++
		}
	}
	f.drops.mu.RUnlock()

	return earlierCount >= maxSlots
}

// autoSelectDropChannel tries to find and add a live channel for a campaign with no matches.
// Returns the login of the added channel, or empty string if none found.
func (f *Farmer) autoSelectDropChannel(campaign twitch.DropCampaign) string {
	if f.spadeSlotsSaturated(campaign) {
		f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Skipping %q — Spade slots saturated by earlier-expiring P0 campaigns", campaign.Name))
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Starting auto-select for campaign %q (game=%q, allowedChannels=%d)",
		campaign.Name, campaign.GameName, len(campaign.Channels)))

	if len(campaign.Channels) > 0 {
		// Campaign has an allowed channels list — must select from those

		// Strategy 1: Query game directory (1 API call) and cross-reference with allowed list
		if campaign.GameName != "" {
			login := f.findAllowedChannelViaDirectory(campaign)
			if login != "" {
				f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Strategy 1 (directory) found %q for %q", login, campaign.Name))
				return login
			}
			f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Strategy 1 (directory) found no match for %q", campaign.Name))
		} else {
			f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Strategy 1 skipped: no GameName for %q", campaign.Name))
		}

		// Strategy 2: Check individual allowed channels (limited to 50 to avoid rate-limiting)
		login := f.findLiveFromAllowedChannels(campaign, "")
		if login != "" {
			f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Strategy 2 (individual) found %q for %q", login, campaign.Name))
			return login
		}
		f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Strategy 2 (individual) found no match for %q", campaign.Name))

		f.addLog("[Drops] No live allowed channel found for %q (%d allowed channels)", campaign.Name, len(campaign.Channels))
		return ""
	}

	// No channel restrictions — any streamer with the game works
	if campaign.GameName != "" {
		login := f.findLiveFromGameDirectory(campaign.GameName)
		if login != "" {
			if err := f.addTemporaryChannel(login, campaign.ID); err == nil {
				f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] Game directory found %q for unrestricted campaign %q", login, campaign.Name))
				return login
			} else {
				f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] addTemporaryChannel(%q) failed: %v", login, err))
			}
		}
		f.addLog("[Drops] No live stream found for game %q (%s)", campaign.GameName, campaign.Name)
	} else {
		f.writeLogFile(fmt.Sprintf("[Drops/AutoSelect] No GameName and no Channels for %q — cannot auto-select", campaign.Name))
	}

	return ""
}

// findAllowedChannelViaDirectory queries the game directory and cross-references
// results with the campaign's allowed channels. Much faster than checking each individually.
//
// Uses the DROPS_ENABLED system filter so a stream that is in the allowed list
// but is currently broadcasting without the drops-enabled tag (Twitch will not
// credit any watch minutes) is filtered out before we ever assign it.
func (f *Farmer) findAllowedChannelViaDirectory(campaign twitch.DropCampaign) string {
	streams, err := f.gql.GetGameStreamsDropsEnabled(campaign.GameName, 100)
	if err != nil {
		f.writeLogFile(fmt.Sprintf("[Drops/Directory] GetGameStreamsDropsEnabled(%q) error: %v", campaign.GameName, err))
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/Directory] Got %d streams for game %q", len(streams), campaign.GameName))

	// Build allowed lookup (by login and by ID)
	allowedByLogin := make(map[string]bool, len(campaign.Channels))
	allowedByID := make(map[string]bool, len(campaign.Channels))
	for _, ch := range campaign.Channels {
		if ch.Name != "" {
			allowedByLogin[strings.ToLower(ch.Name)] = true
		}
		if ch.DisplayName != "" {
			allowedByLogin[strings.ToLower(ch.DisplayName)] = true
		}
		if ch.ID != "" {
			allowedByID[ch.ID] = true
		}
	}

	matched := 0
	for _, stream := range streams {
		login := strings.ToLower(stream.BroadcasterLogin)

		// Check if this stream is in the allowed list
		if !allowedByLogin[login] && !allowedByID[stream.BroadcasterID] {
			continue
		}
		matched++

		// Check if already tracked — if so, reuse it for this campaign
		f.mu.RLock()
		chID, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			f.mu.RLock()
			ch, exists := f.channels[chID]
			f.mu.RUnlock()
			if exists {
				ch.mu.Lock()
				ch.CampaignID = campaign.ID
				ch.mu.Unlock()
				f.writeLogFile(fmt.Sprintf("[Drops/Directory] Reusing already-tracked channel %q for campaign %q", login, campaign.Name))
				return login
			}
			continue
		}

		if err := f.addTemporaryChannel(login, campaign.ID); err == nil {
			return login
		}
		f.writeLogFile(fmt.Sprintf("[Drops/Directory] addTemporaryChannel(%q) failed: %v", login, err))
	}

	f.writeLogFile(fmt.Sprintf("[Drops/Directory] %d streams matched allowed list, none usable for %q", matched, campaign.Name))
	return ""
}

// findLiveFromAllowedChannels checks individual allowed channels to find a live one.
// Limited to 50 channels to avoid rate-limiting. Pass excludeLogin to skip a
// known-bad channel (e.g. during failover so we don't re-select the channel
// that just stalled — without this guard, the failover path picks the failing
// channel as its own replacement, which then deletes the temp channel via
// transferDrop's SetDropInfo+ClearDropInfo collision).
func (f *Farmer) findLiveFromAllowedChannels(campaign twitch.DropCampaign, excludeLogin string) string {
	excludeLogin = strings.ToLower(excludeLogin)
	checked := 0
	skippedEmpty := 0
	skippedOffline := 0
	for _, dropCh := range campaign.Channels {
		if checked >= 50 {
			f.writeLogFile(fmt.Sprintf("[Drops/Individual] Hit 50-channel limit for %q (%d total allowed)", campaign.Name, len(campaign.Channels)))
			break
		}

		login := strings.ToLower(dropCh.Name)
		if login == "" {
			// Fall back to displayName if login is empty
			login = strings.ToLower(dropCh.DisplayName)
		}
		if login == "" {
			skippedEmpty++
			continue
		}
		if login == excludeLogin {
			continue
		}

		// Check if already tracked — reuse for this campaign
		f.mu.RLock()
		chID, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			f.mu.RLock()
			ch, exists := f.channels[chID]
			f.mu.RUnlock()
			if exists && ch.Snapshot().IsOnline {
				ch.mu.Lock()
				ch.CampaignID = campaign.ID
				ch.mu.Unlock()
				f.writeLogFile(fmt.Sprintf("[Drops/Individual] Reusing already-tracked online channel %q for campaign %q", login, campaign.Name))
				return login
			}
			continue
		}

		info, err := f.gql.GetChannelInfo(login)
		if err != nil {
			checked++
			continue
		}
		checked++

		if info.IsLive {
			if err := f.addTemporaryChannel(login, campaign.ID); err == nil {
				return login
			}
			f.writeLogFile(fmt.Sprintf("[Drops/Individual] addTemporaryChannel(%q) failed: %v", login, err))
		} else {
			skippedOffline++
		}

		// Rate limiting: 200ms between GQL calls
		time.Sleep(200 * time.Millisecond)
	}

	if skippedEmpty > 0 || checked > 0 {
		f.writeLogFile(fmt.Sprintf("[Drops/Individual] Campaign %q: checked=%d, offline=%d, emptyLogin=%d",
			campaign.Name, checked, skippedOffline, skippedEmpty))
	}
	return ""
}

// findLiveFromGameDirectory queries the game directory for live streams that
// have the Drops Enabled filter set. Returns the login of the first suitable
// stream, or empty string. We use the drops-enabled filter so we don't pick a
// streamer who is in the game category but not actually running drops — those
// would just sit as a temp channel without making any drop progress.
func (f *Farmer) findLiveFromGameDirectory(gameName string) string {
	streams, err := f.gql.GetGameStreamsDropsEnabled(gameName, 100)
	if err != nil {
		f.addLog("[Drops] Failed to query game directory for %q: %v", gameName, err)
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] Got %d drops-enabled streams for game %q", len(streams), gameName))

	if len(streams) == 0 {
		f.writeLogFile(fmt.Sprintf("[Drops/GameDir] No streams found for game %q", gameName))
		return ""
	}

	// Always pick the first (highest viewer count from VIEWER_COUNT sort).
	login := strings.ToLower(streams[0].BroadcasterLogin)

	f.mu.RLock()
	_, tracked := f.loginMap[login]
	f.mu.RUnlock()
	if tracked {
		f.writeLogFile(fmt.Sprintf("[Drops/GameDir] %q already tracked, returning for reuse", login))
	}
	return login
}

// findLiveFromGameDirectoryExcluding is like findLiveFromGameDirectory but skips a specific login.
// Used during failover to avoid re-selecting the channel that just went offline/raided.
// Like findLiveFromGameDirectory, it filters to drops-enabled streams only.
func (f *Farmer) findLiveFromGameDirectoryExcluding(gameName, excludeLogin string) string {
	streams, err := f.gql.GetGameStreamsDropsEnabled(gameName, 100)
	if err != nil {
		f.addLog("[Drops] Failed to query game directory for %q: %v", gameName, err)
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] Got %d drops-enabled streams for game %q (excluding %q)", len(streams), gameName, excludeLogin))

	for _, stream := range streams {
		login := strings.ToLower(stream.BroadcasterLogin)

		if login == excludeLogin {
			continue
		}

		// Skip if already tracked
		f.mu.RLock()
		_, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			continue
		}

		return login
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] No suitable replacement found for game %q", gameName))
	return ""
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

// pickExclusiveCampaigns selects which exclusive campaigns (≤2 allowed channels) to farm.
// Groups exclusive campaigns by their first unclaimed benefit ID, then picks ONE per group:
// the campaign with the most progress (finishes fastest). Returns a set of picked campaign IDs.
func (f *Farmer) pickExclusiveCampaigns(campaigns []twitch.DropCampaign) map[string]bool {
	// Group exclusive campaigns by first unclaimed benefit ID
	type candidate struct {
		campaignID string
		progress   int // total progress across all drops
	}
	groups := make(map[string][]candidate) // benefitID → candidates

	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if !c.EndAt.IsZero() && c.EndAt.Before(time.Now()) {
			continue
		}
		if f.cfg.IsCampaignDisabled(c.ID) {
			continue
		}
		if f.cfg.IsCampaignCompleted(c.ID) {
			continue
		}
		isExclusive := len(c.Channels) > 0 && len(c.Channels) <= 2
		if !isExclusive {
			continue
		}

		// Find first unclaimed watchable drop's benefit ID
		benefitID := ""
		totalProgress := 0
		for _, drop := range c.Drops {
			if drop.RequiredMinutesWatched <= 0 {
				continue
			}
			totalProgress += drop.CurrentMinutesWatched
			if !drop.IsClaimed && benefitID == "" {
				benefitID = drop.BenefitID
			}
		}
		if benefitID == "" {
			continue // all drops claimed or no benefit ID
		}

		groups[benefitID] = append(groups[benefitID], candidate{c.ID, totalProgress})
	}

	// Pick the best candidate per group (most progress = finishes fastest)
	picked := make(map[string]bool)
	for _, candidates := range groups {
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.progress > best.progress {
				best = c
			}
		}
		picked[best.campaignID] = true
	}

	return picked
}

// verifyTempChannelHealth checks that temporary drop channels are still online.
// PubSub may miss StreamDown events, leaving channels with stale "online" state
// that blocks failover. This verifies via GQL API and triggers failover if needed.
func (f *Farmer) verifyTempChannelHealth() {
	type tempCheck struct {
		chID       string
		login      string
		campaignID string
	}

	f.mu.RLock()
	var toCheck []tempCheck
	for chID, ch := range f.channels {
		snap := ch.Snapshot()
		if snap.IsTemporary && snap.CampaignID != "" && snap.IsOnline {
			toCheck = append(toCheck, tempCheck{chID, ch.Login, snap.CampaignID})
		}
	}
	f.mu.RUnlock()

	if len(toCheck) == 0 {
		return
	}

	for _, tc := range toCheck {
		info, err := f.gql.GetChannelInfo(tc.login)
		if err != nil {
			continue
		}
		if !info.IsLive {
			f.writeLogFile(fmt.Sprintf("[Drops/Health] Temp channel %s is offline (stale state, PubSub missed StreamDown)", tc.login))
			// Update the channel state to offline
			f.mu.RLock()
			if ch, ok := f.channels[tc.chID]; ok {
				ch.mu.Lock()
				ch.IsOnline = false
				ch.mu.Unlock()
			}
			f.mu.RUnlock()
			// Trigger failover to find a replacement
			f.handleDropFailover(tc.chID)
		}
	}
}

// GetActiveDrops returns the union of active+queued+idle drops for the web UI.
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
