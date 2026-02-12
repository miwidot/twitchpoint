package farmer

import (
	"strings"
	"sync"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// ActiveDrop represents a drop being tracked, exposed for the Web UI.
type ActiveDrop struct {
	CampaignName string `json:"campaign_name"`
	GameName     string `json:"game_name"`
	DropName     string `json:"drop_name"`
	ChannelLogin string `json:"channel_login"` // matched configured channel (if any)
	Progress     int    `json:"progress"`       // current minutes watched
	Required     int    `json:"required"`        // minutes required
	Percent      int    `json:"percent"`         // 0-100
	IsClaimed    bool   `json:"is_claimed"`
}

// dropState holds internal state for the drop tracker.
type dropState struct {
	mu          sync.RWMutex
	activeDrops []ActiveDrop
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

	ticker := time.NewTicker(10 * time.Minute)
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

	// Build a set of channelIDs that have active drops this cycle
	activeDropChannels := make(map[string]bool)
	var allDrops []ActiveDrop

	for _, campaign := range campaigns {
		// Build a lookup of campaign channel IDs -> configured channel login
		campaignChannelIDs := f.matchCampaignChannels(campaign)

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

			// Find which configured channel matches this campaign
			matchedLogin := ""
			for chID, login := range campaignChannelIDs {
				activeDropChannels[chID] = true
				if matchedLogin == "" {
					matchedLogin = login
				}

				// Update channel state
				f.mu.RLock()
				if ch, ok := f.channels[chID]; ok {
					ch.SetDropInfo(dropName, drop.CurrentMinutesWatched, drop.RequiredMinutesWatched)
				}
				f.mu.RUnlock()
			}

			allDrops = append(allDrops, ActiveDrop{
				CampaignName: campaign.Name,
				GameName:     campaign.GameName,
				DropName:     dropName,
				ChannelLogin: matchedLogin,
				Progress:     drop.CurrentMinutesWatched,
				Required:     drop.RequiredMinutesWatched,
				Percent:      drop.ProgressPercent(),
				IsClaimed:    false,
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

	// Store active drops for Web UI
	f.drops.mu.Lock()
	f.drops.activeDrops = allDrops
	f.drops.mu.Unlock()

	// Trigger rotation to apply P0 priority for drop channels
	if len(activeDropChannels) > 0 {
		f.rotateChannels()
	}

	if len(allDrops) > 0 {
		f.addLog("[Drops] Tracking %d active drop(s) across %d channel(s)", len(allDrops), len(activeDropChannels))
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
