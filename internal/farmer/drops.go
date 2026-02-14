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
	CampaignID     string    `json:"campaign_id"`
	CampaignName   string    `json:"campaign_name"`
	GameName       string    `json:"game_name"`
	DropName       string    `json:"drop_name"`
	ChannelLogin   string    `json:"channel_login"`    // matched channel (if any)
	Progress       int       `json:"progress"`          // current minutes watched
	Required       int       `json:"required"`           // minutes required
	Percent        int       `json:"percent"`            // 0-100
	IsClaimed      bool      `json:"is_claimed"`
	EndAt          time.Time `json:"end_at"`             // campaign end time
	IsAutoSelected bool      `json:"is_auto_selected"`   // channel was auto-discovered
	IsEnabled      bool      `json:"is_enabled"`          // campaign not disabled
}

// dropState holds internal state for the drop tracker.
type dropState struct {
	mu            sync.RWMutex
	activeDrops   []ActiveDrop
	campaignCache map[string]twitch.DropCampaign // campaignID -> campaign, rebuilt each cycle

	failoverCooldowns map[string]time.Time // campaignID -> last failover attempt
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

// processDrops fetches the inventory, updates channel drop state, and auto-claims completed drops.
func (f *Farmer) processDrops() {
	campaigns, err := f.gql.GetDropsInventory()
	if err != nil {
		f.addLog("[Drops] Failed to fetch inventory: %v", err)
		return
	}

	// Rebuild campaign cache for failover lookups
	newCache := make(map[string]twitch.DropCampaign)

	// Track which campaigns are still active (for temp channel cleanup)
	activeCampaignIDs := make(map[string]bool)

	// Build a set of channelIDs that have active drops this cycle
	activeDropChannels := make(map[string]bool)
	var allDrops []ActiveDrop

	for _, campaign := range campaigns {
		// Skip expired campaigns (Twitch may return stale data)
		if !campaign.EndAt.IsZero() && campaign.EndAt.Before(time.Now()) {
			continue
		}

		// Skip disabled campaigns
		if f.cfg.IsCampaignDisabled(campaign.ID) {
			continue
		}

		newCache[campaign.ID] = campaign
		activeCampaignIDs[campaign.ID] = true

		// Build a lookup of campaign channel IDs -> configured channel login
		campaignChannelIDs := f.matchCampaignChannels(campaign)

		// Auto-select: if no channels match, try to find a live one
		if len(campaignChannelIDs) == 0 {
			autoLogin := f.autoSelectDropChannel(campaign)
			if autoLogin != "" {
				// Direct lookup — don't re-match because game directory channels
				// won't appear in campaign.Channels and matchCampaignChannels would miss them
				f.mu.RLock()
				if chID, ok := f.loginMap[autoLogin]; ok {
					campaignChannelIDs[chID] = autoLogin
				}
				f.mu.RUnlock()
			}
		}

		for _, drop := range campaign.Drops {
			if drop.IsClaimed {
				continue
			}

			dropName := drop.BenefitName
			if dropName == "" {
				dropName = drop.Name
			}

			// Check if this drop is complete and can be claimed
			if drop.IsComplete() && drop.DropInstanceID != "" {
				f.addLog("[Drops] Claiming completed drop: %s (%s)", dropName, campaign.Name)
				go func(instanceID, name string) {
					if err := f.gql.ClaimDrop(instanceID); err != nil {
						f.addLog("[Drops] Failed to claim %s: %v", name, err)
					} else {
						f.addLog("[Drops] Claimed: %s", name)
					}
				}(drop.DropInstanceID, dropName)
				continue
			}

			// Pick the single best channel for this drop:
			// watching > online > offline, prefer non-temporary
			bestChID := ""
			bestLogin := ""
			bestScore := -1
			isAutoSelected := false

			f.mu.RLock()
			for chID, login := range campaignChannelIDs {
				ch, ok := f.channels[chID]
				if !ok {
					continue
				}
				snap := ch.Snapshot()
				score := 0
				if snap.IsOnline {
					score = 1
				}
				if snap.IsWatching {
					score = 2
				}
				// Prefer non-temporary channels at equal score
				if !snap.IsTemporary && score == bestScore {
					score++
				}
				if score > bestScore {
					bestScore = score
					bestChID = chID
					bestLogin = login
				}
			}
			f.mu.RUnlock()

			if bestChID != "" {
				activeDropChannels[bestChID] = true
				f.mu.RLock()
				if ch, ok := f.channels[bestChID]; ok {
					ch.SetDropInfo(dropName, drop.CurrentMinutesWatched, drop.RequiredMinutesWatched)
					ch.mu.Lock()
					ch.CampaignID = campaign.ID
					ch.mu.Unlock()
					isAutoSelected = ch.Snapshot().IsTemporary
				}
				f.mu.RUnlock()
			}

			allDrops = append(allDrops, ActiveDrop{
				CampaignID:     campaign.ID,
				CampaignName:   campaign.Name,
				GameName:       campaign.GameName,
				DropName:       dropName,
				ChannelLogin:   bestLogin,
				Progress:       drop.CurrentMinutesWatched,
				Required:       drop.RequiredMinutesWatched,
				Percent:        drop.ProgressPercent(),
				IsClaimed:      false,
				EndAt:          campaign.EndAt,
				IsAutoSelected: isAutoSelected,
				IsEnabled:      true,
			})
		}
	}

	// Clear drops for channels no longer in any active campaign
	f.mu.RLock()
	for chID, ch := range f.channels {
		if !activeDropChannels[chID] {
			snap := ch.Snapshot()
			if snap.HasActiveDrop {
				ch.ClearDropInfo()
			}
		}
	}
	f.mu.RUnlock()

	// Remove stale temporary channels no longer serving an active drop
	f.cleanupTemporaryChannels(activeCampaignIDs)

	// Store campaign cache and active drops
	f.drops.mu.Lock()
	f.drops.activeDrops = allDrops
	f.drops.campaignCache = newCache
	f.drops.mu.Unlock()

	// Clean old failover cooldown entries
	f.cleanupFailoverCooldowns()

	// Trigger rotation to apply P0 priority for drop channels
	if len(activeDropChannels) > 0 {
		f.rotateChannels()
	}

	if len(allDrops) > 0 {
		f.addLog("[Drops] Tracking %d active drop(s) across %d channel(s)", len(allDrops), len(activeDropChannels))
	}
}

// cleanupTemporaryChannels removes temp channels that are no longer useful:
// - campaign ended or was disabled (not in activeCampaignIDs)
// - channel went offline and has no active drop progress
func (f *Farmer) cleanupTemporaryChannels(activeCampaignIDs map[string]bool) {
	f.mu.RLock()
	var staleChannels []string
	for chID, ch := range f.channels {
		snap := ch.Snapshot()
		if !snap.IsTemporary {
			continue
		}
		// Campaign no longer active (ended/disabled/claimed)
		if snap.CampaignID != "" && !activeCampaignIDs[snap.CampaignID] {
			staleChannels = append(staleChannels, chID)
			continue
		}
		// Offline and not actively tracking a drop — dead weight
		if !snap.IsOnline && !snap.HasActiveDrop {
			staleChannels = append(staleChannels, chID)
			continue
		}
		// Zombie: temp channel lost its campaign link and has no active drop
		if snap.CampaignID == "" && !snap.HasActiveDrop {
			staleChannels = append(staleChannels, chID)
		}
	}
	f.mu.RUnlock()

	for _, chID := range staleChannels {
		f.removeTemporaryChannel(chID)
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

// autoSelectDropChannel tries to find and add a live channel for a campaign with no matches.
// Returns the login of the added channel, or empty string if none found.
func (f *Farmer) autoSelectDropChannel(campaign twitch.DropCampaign) string {
	// Try allowed channels first
	if len(campaign.Channels) > 0 {
		login := f.findLiveFromAllowedChannels(campaign)
		if login != "" {
			return login
		}
	}

	// Fall back to game directory
	if campaign.GameName != "" {
		login := f.findLiveFromGameDirectory(campaign.GameName)
		if login != "" {
			if err := f.addTemporaryChannel(login, campaign.ID); err == nil {
				return login
			}
		}
	}

	return ""
}

// findLiveFromAllowedChannels iterates campaign's allowed channel list,
// checking each via GetChannelInfo to find one that's live.
// Adds as temporary channel if found. Returns the login or empty string.
func (f *Farmer) findLiveFromAllowedChannels(campaign twitch.DropCampaign) string {
	for _, dropCh := range campaign.Channels {
		login := strings.ToLower(dropCh.Name)
		if login == "" {
			continue
		}

		// Skip if already tracked
		f.mu.RLock()
		_, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			continue
		}

		info, err := f.gql.GetChannelInfo(login)
		if err != nil {
			continue
		}
		if info.IsLive {
			if err := f.addTemporaryChannel(login, campaign.ID); err == nil {
				return login
			}
		}

		// Rate limiting: 200ms between GQL calls
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// findLiveFromGameDirectory queries the game directory for live streams.
// Returns the login of the first suitable stream, or empty string.
func (f *Farmer) findLiveFromGameDirectory(gameName string) string {
	streams, err := f.gql.GetGameStreams(gameName, 10)
	if err != nil {
		f.addLog("[Drops] Failed to query game directory for %q: %v", gameName, err)
		return ""
	}

	for _, stream := range streams {
		login := strings.ToLower(stream.BroadcasterLogin)

		// Skip if already tracked
		f.mu.RLock()
		_, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			continue
		}

		return login
	}
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

// GetActiveDrops returns the current active drops for the Web UI.
func (f *Farmer) GetActiveDrops() []ActiveDrop {
	f.drops.mu.RLock()
	defer f.drops.mu.RUnlock()

	if len(f.drops.activeDrops) == 0 {
		return nil
	}

	result := make([]ActiveDrop, len(f.drops.activeDrops))
	copy(result, f.drops.activeDrops)
	return result
}
