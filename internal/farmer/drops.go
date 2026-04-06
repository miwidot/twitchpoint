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

	// Sort campaigns by end time — soonest expiring first so they get channel priority
	sort.Slice(campaigns, func(i, j int) bool {
		ei, ej := campaigns[i].EndAt, campaigns[j].EndAt
		// Zero end time (unknown) goes last
		if ei.IsZero() {
			return false
		}
		if ej.IsZero() {
			return true
		}
		return ei.Before(ej)
	})

	// Rebuild campaign cache for failover lookups
	newCache := make(map[string]twitch.DropCampaign)

	// Track which campaigns are still active (for temp channel cleanup)
	activeCampaignIDs := make(map[string]bool)

	// Build a set of channelIDs that have active drops this cycle
	activeDropChannels := make(map[string]bool)
	var allDrops []ActiveDrop

	f.writeLogFile(fmt.Sprintf("[Drops] Inventory returned %d campaigns", len(campaigns)))

	// Debug: log claimed status for connected campaigns to verify gameEventDrops merge
	for _, campaign := range campaigns {
		if campaign.IsAccountConnected {
			for _, drop := range campaign.Drops {
				if drop.RequiredMinutesWatched > 0 {
					f.writeLogFile(fmt.Sprintf("[Drops/Debug] %q drop %q: benefitID=%q claimed=%v progress=%d/%d",
						campaign.Name, drop.Name, drop.BenefitID, drop.IsClaimed, drop.CurrentMinutesWatched, drop.RequiredMinutesWatched))
				}
			}
		}
	}

	for _, campaign := range campaigns {
		f.writeLogFile(fmt.Sprintf("[Drops] Processing campaign %q (game=%q, status=%q, drops=%d, endsAt=%s, connected=%v)",
			campaign.Name, campaign.GameName, campaign.Status, len(campaign.Drops), campaign.EndAt.Format("2006-01-02 15:04"), campaign.IsAccountConnected))

		// Skip non-ACTIVE campaigns (inventory returns ACTIVE and EXPIRED)
		if campaign.Status != "" && campaign.Status != "ACTIVE" {
			f.writeLogFile(fmt.Sprintf("[Drops] Skipping non-active campaign %q (status=%s)", campaign.Name, campaign.Status))
			continue
		}

		// Skip expired campaigns (Twitch may return stale data)
		if !campaign.EndAt.IsZero() && campaign.EndAt.Before(time.Now()) {
			f.writeLogFile(fmt.Sprintf("[Drops] Skipping expired campaign %q", campaign.Name))
			continue
		}

		// Skip disabled campaigns
		if f.cfg.IsCampaignDisabled(campaign.ID) {
			f.writeLogFile(fmt.Sprintf("[Drops] Skipping disabled campaign %q", campaign.Name))
			continue
		}

		// Skip campaigns where account is not linked (drops won't be credited)
		if !campaign.IsAccountConnected {
			continue
		}

		// Skip campaigns we've already fully claimed (tracked in config)
		if f.cfg.IsCampaignCompleted(campaign.ID) {
			f.writeLogFile(fmt.Sprintf("[Drops] Skipping completed campaign %q", campaign.Name))
			continue
		}

		newCache[campaign.ID] = campaign
		activeCampaignIDs[campaign.ID] = true

		// Build a lookup of campaign channel IDs -> configured channel login
		campaignChannelIDs := f.matchCampaignChannels(campaign)

		// Auto-select: if no channels match OR none are online, try to find a live one
		needsAutoSelect := len(campaignChannelIDs) == 0
		if !needsAutoSelect {
			// Check if any matched channel is actually online
			hasOnline := false
			f.mu.RLock()
			for chID := range campaignChannelIDs {
				if ch, ok := f.channels[chID]; ok && ch.Snapshot().IsOnline {
					hasOnline = true
					break
				}
			}
			f.mu.RUnlock()
			needsAutoSelect = !hasOnline
		}

		f.writeLogFile(fmt.Sprintf("[Drops] Campaign %q: matchedChannels=%d, needsAutoSelect=%v",
			campaign.Name, len(campaignChannelIDs), needsAutoSelect))

		if needsAutoSelect {
			autoLogin := f.autoSelectDropChannel(campaign)
			if autoLogin != "" {
				// Direct lookup — don't re-match because game directory channels
				// won't appear in campaign.Channels and matchCampaignChannels would miss them
				f.mu.RLock()
				if chID, ok := f.loginMap[autoLogin]; ok {
					campaignChannelIDs[chID] = autoLogin
				}
				f.mu.RUnlock()
			} else {
				f.writeLogFile(fmt.Sprintf("[Drops] Auto-select returned empty for campaign %q", campaign.Name))
			}
		}

		// Check if all watchable drops are claimed → mark campaign as completed
		allClaimed := true
		hasWatchableDrops := false
		for _, drop := range campaign.Drops {
			if drop.RequiredMinutesWatched <= 0 {
				continue // sub-only, ignore
			}
			hasWatchableDrops = true
			if !drop.IsClaimed {
				allClaimed = false
				break
			}
		}
		if hasWatchableDrops && allClaimed {
			f.cfg.MarkCampaignCompleted(campaign.ID)
			f.cfg.Save()
			source := "inventory"
			if !campaign.InInventory {
				source = "gameEventDrops"
			}
			f.addLog("[Drops] Campaign %q fully claimed (detected via %s) — marked as completed", campaign.Name, source)
			continue
		}

		for _, drop := range campaign.Drops {
			if drop.IsClaimed {
				continue
			}

			// Skip sub-only drops (0 required minutes = can't be earned by watching)
			if drop.RequiredMinutesWatched <= 0 {
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
				CampaignID:         campaign.ID,
				CampaignName:       campaign.Name,
				GameName:           campaign.GameName,
				DropName:           dropName,
				ChannelLogin:       bestLogin,
				Progress:           drop.CurrentMinutesWatched,
				Required:           drop.RequiredMinutesWatched,
				Percent:            drop.ProgressPercent(),
				IsClaimed:          false,
				EndAt:              campaign.EndAt,
				IsAutoSelected:     isAutoSelected,
				IsEnabled:          true,
				IsAccountConnected: true,
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
		login := f.findLiveFromAllowedChannels(campaign)
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
func (f *Farmer) findAllowedChannelViaDirectory(campaign twitch.DropCampaign) string {
	streams, err := f.gql.GetGameStreams(campaign.GameName, 100)
	if err != nil {
		f.writeLogFile(fmt.Sprintf("[Drops/Directory] GetGameStreams(%q) error: %v", campaign.GameName, err))
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
// Limited to 50 channels to avoid rate-limiting.
func (f *Farmer) findLiveFromAllowedChannels(campaign twitch.DropCampaign) string {
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

// findLiveFromGameDirectory queries the game directory for live streams.
// Returns the login of the first suitable stream, or empty string.
func (f *Farmer) findLiveFromGameDirectory(gameName string) string {
	streams, err := f.gql.GetGameStreams(gameName, 100)
	if err != nil {
		f.addLog("[Drops] Failed to query game directory for %q: %v", gameName, err)
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] Got %d streams for game %q", len(streams), gameName))

	for _, stream := range streams {
		login := strings.ToLower(stream.BroadcasterLogin)

		// If already tracked, reuse it (caller will set campaign ID)
		f.mu.RLock()
		_, tracked := f.loginMap[login]
		f.mu.RUnlock()
		if tracked {
			f.writeLogFile(fmt.Sprintf("[Drops/GameDir] %q already tracked, returning for reuse", login))
			return login
		}

		return login
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] No streams found for game %q", gameName))
	return ""
}

// findLiveFromGameDirectoryExcluding is like findLiveFromGameDirectory but skips a specific login.
// Used during failover to avoid re-selecting the channel that just went offline/raided.
func (f *Farmer) findLiveFromGameDirectoryExcluding(gameName, excludeLogin string) string {
	streams, err := f.gql.GetGameStreams(gameName, 100)
	if err != nil {
		f.addLog("[Drops] Failed to query game directory for %q: %v", gameName, err)
		return ""
	}

	f.writeLogFile(fmt.Sprintf("[Drops/GameDir] Got %d streams for game %q (excluding %q)", len(streams), gameName, excludeLogin))

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
