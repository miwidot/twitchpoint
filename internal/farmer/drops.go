package farmer

import (
	"fmt"
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
