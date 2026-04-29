package farmer

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// LogEntry represents a single log line in the event log.
type LogEntry struct {
	Time    time.Time
	Message string
}

// Farmer is the main orchestrator that ties GQL, PubSub, Spade, and IRC together.
type Farmer struct {
	cfg     *config.Config
	version string
	gql     *twitch.GQLClient
	pubsub  *twitch.PubSubClient
	spade   *twitch.SpadeTracker
	irc     *twitch.IRCClient
	events  chan twitch.FarmerEvent

	user *twitch.UserInfo

	mu       sync.RWMutex
	channels map[string]*ChannelState // channelID -> state
	loginMap map[string]string        // login -> channelID

	logMu      sync.RWMutex
	logEntries []LogEntry
	logFile    *os.File
	logDate    string // current log file date (YYYY-MM-DD) for rotation

	startTime time.Time
	stopCh    chan struct{}
	stopped   bool

	// Stats
	totalPointsEarned int
	totalClaimsMade   int

	// Dedup
	seenClaims map[string]time.Time // claimID -> when we attempted
	seenRaids  map[string]time.Time // raidID -> when we attempted

	// Name cache for untracked channels (PubSub fires for all channels user watches)
	nameCache map[string]string // channelID -> displayName

	// Rotation
	rotationIndex int // which pair of channels is currently being watched

	// Drops
	drops dropState

	// Update checker
	update updateState
}

// New creates a new Farmer from config.
func New(cfg *config.Config, version string) *Farmer {
	return &Farmer{
		cfg:        cfg,
		version:    version,
		events:     make(chan twitch.FarmerEvent, 100),
		channels:   make(map[string]*ChannelState),
		loginMap:   make(map[string]string),
		seenClaims: make(map[string]time.Time),
		nameCache:  make(map[string]string),
		seenRaids:  make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
}

// Start initializes all subsystems and begins farming.
func (f *Farmer) Start() error {
	f.startTime = time.Now()

	// Open daily debug log file (append mode)
	if err := os.MkdirAll("logs", 0755); err != nil {
		return fmt.Errorf("create logs dir: %w", err)
	}
	logPath := fmt.Sprintf("logs/debug-%s.log", time.Now().Format("2006-01-02"))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	f.logFile = logFile
	f.logDate = time.Now().Format("2006-01-02")
	f.writeLogFile("=== TwitchPoint Farmer started ===")

	// Initialize GQL client
	f.gql = twitch.NewGQLClient(f.cfg.AuthToken)

	// Initialize drop selector now that gql client exists
	f.drops.selector = NewDropSelector(f.cfg, f.gql)

	// Validate auth token by getting user info
	user, err := f.gql.GetUserInfo()
	if err != nil {
		return fmt.Errorf("auth validation failed: %w", err)
	}
	f.user = user
	f.addLog("Logged in as %s (ID: %s)", user.DisplayName, user.ID)

	// Initialize Spade tracker
	f.spade = twitch.NewSpadeTracker(user.ID, f.cfg.AuthToken, f.addLog)
	if err := f.spade.Start(); err != nil {
		f.addLog("Spade initialization warning: %v", err)
	}

	// Initialize PubSub
	f.pubsub = twitch.NewPubSubClient(f.cfg.AuthToken, f.events)

	// Subscribe to user-level PubSub topics: community points + v1.8.0 drop events
	if err := f.pubsub.Listen([]string{
		fmt.Sprintf("community-points-user-v1.%s", user.ID),
		fmt.Sprintf("user-drop-events.%s", user.ID),
	}); err != nil {
		f.addLog("PubSub user topic error: %v", err)
	}

	// Initialize IRC for viewer presence
	if f.cfg.IrcEnabled {
		f.irc = twitch.NewIRCClient(f.cfg.AuthToken, user.Login, f.addLog)
	}

	// Initialize channels first (stores all PubSub topics before connecting)
	for _, entry := range f.cfg.GetChannelEntries() {
		if err := f.addChannelFromEntry(entry); err != nil {
			f.addLog("Failed to add channel %s: %v", entry.Login, err)
		}
	}

	// Start event loop before PubSub connect so events are processed immediately
	go f.eventLoop()

	// Connect PubSub AFTER all channels are added — subscribes to all topics at once
	go f.pubsub.Connect()

	// Connect IRC for viewer presence
	if f.irc != nil {
		go f.irc.Connect()
	}

	// Start periodic balance refresh
	go f.balanceRefreshLoop()

	// Start channel rotation (Twitch only credits points for 2 channels at a time)
	go f.rotationLoop()

	// Start drop mining if enabled
	if f.cfg.DropsEnabled {
		f.addLog("Drop mining enabled — checking inventory every 15 min + DropCurrentSession poll every 60s")
		go f.dropCheckLoop()
		go f.dropProgressPollLoop()
	}

	// Start background update checker
	go f.updateCheckLoop()

	return nil
}

// Stop shuts down the farmer.
func (f *Farmer) Stop() {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return
	}
	f.stopped = true
	close(f.stopCh)
	f.mu.Unlock()

	f.writeLogFile("=== TwitchPoint Farmer stopped ===")

	if f.pubsub != nil {
		f.pubsub.Close()
	}
	if f.irc != nil {
		f.irc.Close()
	}
	if f.spade != nil {
		f.spade.Stop()
	}
	if f.logFile != nil {
		f.logFile.Close()
	}
}

// Done returns a channel that is closed when the farmer stops.
func (f *Farmer) Done() <-chan struct{} {
	return f.stopCh
}

// addChannelFromEntry resolves a channel from config, using ID if available (handles renames).
func (f *Farmer) addChannelFromEntry(entry config.ChannelEntry) error {
	var info *twitch.ChannelInfo
	var err error

	if entry.ID != "" {
		// Resolve by ID — handles channel renames
		info, err = f.gql.GetChannelInfoByID(entry.ID)
		if err != nil {
			// Fallback to login if ID lookup fails
			info, err = f.gql.GetChannelInfo(entry.Login)
		} else if info.Login != entry.Login {
			// Channel was renamed — update config
			f.addLog("Channel renamed: %s → %s (ID: %s)", entry.Login, info.Login, info.ID)
			f.cfg.UpdateChannelLogin(entry.ID, info.Login)
			f.cfg.Save()
		}
	} else {
		// No ID stored — resolve by login and persist the ID
		info, err = f.gql.GetChannelInfo(entry.Login)
		if err == nil {
			f.cfg.SetChannelID(entry.Login, info.ID)
			f.cfg.Save()
		}
	}

	if err != nil {
		return fmt.Errorf("get channel info: %w", err)
	}

	return f.addChannelWithInfo(info)
}

func (f *Farmer) addChannelWithInfo(info *twitch.ChannelInfo) error {
	state := NewChannelState(info.Login, info.DisplayName, info.ID)
	state.Priority = f.cfg.GetPriority(info.Login)

	f.mu.Lock()
	f.channels[info.ID] = state
	f.loginMap[info.Login] = info.ID
	f.mu.Unlock()

	// Subscribe to PubSub topics for this channel
	topics := []string{
		fmt.Sprintf("video-playback-by-id.%s", info.ID),
		fmt.Sprintf("raid.%s", info.ID),
	}
	if err := f.pubsub.Listen(topics); err != nil {
		f.addLog("PubSub subscribe error for %s: %v", info.Login, err)
	}

	// Join IRC channel for viewer presence
	if f.irc != nil {
		f.irc.Join(info.Login)
	}

	priLabel := "rotate"
	if state.Priority == 1 {
		priLabel = "PRIORITY"
	}
	f.addLog("Added channel: %s (ID: %s) [%s]", info.DisplayName, info.ID, priLabel)

	// Check if live and start watching
	if info.IsLive {
		state.SetOnline(info.BroadcastID, info.GameName, info.ViewerCount)
		f.addLog("%s is LIVE - %s (%d viewers)", info.DisplayName, info.GameName, info.ViewerCount)
		f.tryStartWatching(state)
	} else {
		f.addLog("%s is offline", info.DisplayName)
	}

	// Fetch initial balance
	channelLogin := info.Login
	go func() {
		balance, err := f.gql.GetChannelPointsBalance(channelLogin)
		if err == nil && balance > 0 {
			state.SetBalance(balance)
			f.addLog("%s balance: %d points", info.DisplayName, balance)
		}
	}()

	return nil
}

// addTemporaryChannel adds a channel for drop tracking without saving to config.
func (f *Farmer) addTemporaryChannel(login, campaignID string) error {
	login = strings.ToLower(login)

	// Check if already tracked
	f.mu.RLock()
	if chID, ok := f.loginMap[login]; ok {
		ch := f.channels[chID]
		f.mu.RUnlock()
		// If it's already a permanent channel, just set the campaign ID
		snap := ch.Snapshot()
		if !snap.IsTemporary {
			ch.mu.Lock()
			ch.CampaignID = campaignID
			ch.mu.Unlock()
			return nil
		}
		return fmt.Errorf("channel %s already tracked", login)
	}
	f.mu.RUnlock()

	info, err := f.gql.GetChannelInfo(login)
	if err != nil {
		return fmt.Errorf("get channel info: %w", err)
	}

	if !info.IsLive {
		return fmt.Errorf("channel %s is not live", login)
	}

	state := NewChannelState(info.Login, info.DisplayName, info.ID)
	state.Priority = 2 // temp channels use P2 (drops will promote to P0)
	state.IsTemporary = true
	state.CampaignID = campaignID

	f.mu.Lock()
	f.channels[info.ID] = state
	f.loginMap[info.Login] = info.ID
	f.mu.Unlock()

	// Subscribe to PubSub topics
	topics := []string{
		fmt.Sprintf("video-playback-by-id.%s", info.ID),
		fmt.Sprintf("raid.%s", info.ID),
	}
	if err := f.pubsub.Listen(topics); err != nil {
		f.addLog("[Drops] PubSub subscribe error for temp channel %s: %v", login, err)
	}

	// Join IRC
	if f.irc != nil {
		f.irc.Join(info.Login)
	}

	state.SetOnline(info.BroadcastID, info.GameName, info.ViewerCount)
	f.addLog("[Drops] Auto-added temporary channel: %s (campaign: %s)", info.DisplayName, campaignID)
	f.tryStartWatching(state)

	return nil
}

// removeTemporaryChannel cleans up a temporary channel without touching config.
func (f *Farmer) removeTemporaryChannel(channelID string) {
	f.mu.Lock()
	ch, ok := f.channels[channelID]
	if !ok {
		f.mu.Unlock()
		return
	}

	login := ch.Login
	displayName := ch.DisplayName
	delete(f.channels, channelID)
	delete(f.loginMap, login)
	f.mu.Unlock()

	f.spade.StopWatching(channelID)

	f.pubsub.Unlisten([]string{
		fmt.Sprintf("video-playback-by-id.%s", channelID),
		fmt.Sprintf("raid.%s", channelID),
	})

	if f.irc != nil {
		f.irc.Part(login)
	}

	f.addLog("[Drops] Removed temporary channel: %s", displayName)
}

// AddChannelLive adds a channel at runtime.
func (f *Farmer) AddChannelLive(login string) error {
	login = strings.ToLower(login)

	f.mu.RLock()
	if chID, ok := f.loginMap[login]; ok {
		ch := f.channels[chID]
		f.mu.RUnlock()

		// If channel exists as temporary, promote to permanent
		if ch.Snapshot().IsTemporary {
			ch.mu.Lock()
			ch.IsTemporary = false
			ch.mu.Unlock()
			f.cfg.AddChannel(login)
			f.cfg.SetChannelID(login, chID)
			if err := f.cfg.Save(); err != nil {
				f.addLog("Warning: could not save config: %v", err)
			}
			f.addLog("Promoted temporary channel %s to permanent", ch.DisplayName)
			return nil
		}
		return fmt.Errorf("channel %s already added", login)
	}
	f.mu.RUnlock()

	// Resolve channel info first so we have the ID
	info, err := f.gql.GetChannelInfo(login)
	if err != nil {
		return fmt.Errorf("get channel info: %w", err)
	}

	// Save to config with ID
	f.cfg.AddChannel(info.Login)
	f.cfg.SetChannelID(info.Login, info.ID)
	if err := f.cfg.Save(); err != nil {
		f.addLog("Warning: could not save config: %v", err)
	}

	return f.addChannelWithInfo(info)
}

// RemoveChannelLive removes a channel at runtime.
func (f *Farmer) RemoveChannelLive(login string) error {
	login = strings.ToLower(login)

	f.mu.RLock()
	channelID, ok := f.loginMap[login]
	if !ok {
		f.mu.RUnlock()
		return fmt.Errorf("channel %s not found", login)
	}
	ch := f.channels[channelID]
	isTemp := ch.Snapshot().IsTemporary
	f.mu.RUnlock()

	// Temporary channels use separate cleanup (no config changes)
	if isTemp {
		f.removeTemporaryChannel(channelID)
		return nil
	}

	f.mu.Lock()
	delete(f.channels, channelID)
	delete(f.loginMap, login)
	f.mu.Unlock()

	// Stop watching
	f.spade.StopWatching(channelID)

	// Unsubscribe PubSub
	f.pubsub.Unlisten([]string{
		fmt.Sprintf("video-playback-by-id.%s", channelID),
		fmt.Sprintf("raid.%s", channelID),
	})

	// Leave IRC channel
	if f.irc != nil {
		f.irc.Part(login)
	}

	f.addLog("Removed channel: %s", ch.DisplayName)

	// Save config
	f.cfg.RemoveChannel(login)
	if err := f.cfg.Save(); err != nil {
		f.addLog("Warning: could not save config: %v", err)
	}

	return nil
}

// SetPriorityLive changes a channel's priority at runtime.
func (f *Farmer) SetPriorityLive(login string, priority int) error {
	login = strings.ToLower(login)
	f.mu.RLock()
	channelID, ok := f.loginMap[login]
	if !ok {
		f.mu.RUnlock()
		return fmt.Errorf("channel %s not found", login)
	}
	ch := f.channels[channelID]
	f.mu.RUnlock()

	ch.mu.Lock()
	ch.Priority = priority
	ch.mu.Unlock()

	priLabel := "rotate"
	if priority == 1 {
		priLabel = "PRIORITY"
	}
	f.addLog("Set %s to %s", ch.DisplayName, priLabel)

	// Save to config
	f.cfg.SetPriority(login, priority)
	if err := f.cfg.Save(); err != nil {
		f.addLog("Warning: could not save config: %v", err)
	}

	// Trigger immediate rotation to apply new priority
	go f.rotateChannels()

	return nil
}

func (f *Farmer) tryStartWatching(state *ChannelState) {
	snap := state.Snapshot()
	if !snap.IsOnline || snap.IsWatching {
		return
	}

	if snap.BroadcastID == "" {
		f.addLog("[Spade] skipping %s — no broadcast ID", snap.DisplayName)
		return
	}

	if f.spade.StartWatching(snap.ChannelID, snap.Login, snap.BroadcastID) {
		state.SetWatching(true)
		f.addLog("Started watching %s (Spade active, broadcast=%s)", snap.DisplayName, snap.BroadcastID)
	}
}

func (f *Farmer) eventLoop() {
	for {
		select {
		case evt := <-f.events:
			f.handleEvent(evt)
		case <-f.stopCh:
			return
		}
	}
}

func (f *Farmer) handleEvent(evt twitch.FarmerEvent) {
	f.mu.RLock()
	ch, ok := f.channels[evt.ChannelID]
	f.mu.RUnlock()

	switch evt.Type {
	case twitch.EventClaimAvailable:
		data := evt.Data.(twitch.ClaimData)

		// Dedup - only attempt each claim once
		f.mu.Lock()
		if _, seen := f.seenClaims[data.ClaimID]; seen {
			f.mu.Unlock()
			return
		}
		f.seenClaims[data.ClaimID] = time.Now()
		// Clean old entries
		for id, t := range f.seenClaims {
			if time.Since(t) > 30*time.Minute {
				delete(f.seenClaims, id)
			}
		}
		f.mu.Unlock()

		// Resolve channel name
		channelName := evt.ChannelID
		claimCh := ch // from top-level lookup
		if ok {
			channelName = ch.DisplayName
		} else {
			// Untracked channel — check name cache or resolve via GQL
			channelName = f.resolveChannelName(evt.ChannelID)
		}

		// Attempt claim with retry
		go func() {
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				if attempt > 0 {
					time.Sleep(2 * time.Second)
				}
				lastErr = f.gql.ClaimCommunityPoints(evt.ChannelID, data.ClaimID)
				if lastErr == nil {
					if claimCh != nil {
						claimCh.RecordClaim()
					}
					f.mu.Lock()
					f.totalClaimsMade++
					f.mu.Unlock()
					f.addLog("Claimed bonus on %s!", channelName)
					return
				}
			}
			f.addLog("Claim failed on %s after 3 attempts: %v", channelName, lastErr)
		}()

	case twitch.EventPointsEarned:
		data := evt.Data.(twitch.PointsData)
		f.mu.Lock()
		f.totalPointsEarned += data.PointsGained
		f.mu.Unlock()
		if ok {
			ch.AddPointsEarned(data.PointsGained, data.TotalPoints)
			f.addLog("+%d points on %s (%s) - Balance: %d",
				data.PointsGained, ch.DisplayName, data.ReasonCode, data.TotalPoints)
		} else {
			channelName := f.resolveChannelName(evt.ChannelID)
			f.addLog("+%d points on %s (%s) - Balance: %d",
				data.PointsGained, channelName, data.ReasonCode, data.TotalPoints)
		}

	case twitch.EventStreamUp:
		if ok {
			// Fetch fresh stream info with retry for broadcast ID and game
			go func() {
				var broadcastID, gameName string
				for attempt := 0; attempt < 3; attempt++ {
					if attempt > 0 {
						time.Sleep(5 * time.Second) // Wait for Twitch API to update
					}
					info, err := f.gql.GetChannelInfo(ch.Login)
					if err != nil {
						f.addLog("Error fetching stream info for %s (attempt %d): %v", ch.Login, attempt+1, err)
						continue
					}
					ch.SetOnline(info.BroadcastID, info.GameName, info.ViewerCount)
					broadcastID = info.BroadcastID
					gameName = info.GameName
					if broadcastID != "" && gameName != "" {
						break
					}
				}
				if broadcastID == "" {
					f.addLog("%s went LIVE but broadcast ID is empty — heartbeats won't work!", ch.DisplayName)
				} else {
					f.addLog("%s went LIVE! %s (broadcast=%s)", ch.DisplayName, gameName, broadcastID)
				}
				f.tryStartWatching(ch)
			}()
		}

	case twitch.EventStreamDown:
		if ok {
			snap := ch.Snapshot()
			hasDropBefore := snap.HasActiveDrop
			campaignID := snap.CampaignID

			ch.SetOffline()
			f.spade.StopWatching(ch.ChannelID)
			f.addLog("%s went OFFLINE", ch.DisplayName)

			// v1.7.0: drop replacement is handled by the next selector cycle (≤5 min).
			// No more synchronous failover state machine — selector picks fresh from
			// the live drops-enabled pool every cycle, so a dead channel just means
			// "no drop assigned for up to one cycle" rather than orphaned state.
			_ = hasDropBefore
			_ = campaignID

			// Try to fill freed Spade slot
			f.fillSpadeSlots()
		}

	case twitch.EventRaid:
		data := evt.Data.(twitch.RaidData)

		// Only attempt each raid once - PubSub fires this event every second during countdown
		f.mu.Lock()
		if _, seen := f.seenRaids[data.RaidID]; seen {
			f.mu.Unlock()
			return
		}
		f.seenRaids[data.RaidID] = time.Now()
		// Clean up old entries (older than 30 min)
		for id, t := range f.seenRaids {
			if time.Since(t) > 30*time.Minute {
				delete(f.seenRaids, id)
			}
		}
		f.mu.Unlock()

		sourceName := evt.ChannelID
		if ok {
			sourceName = ch.DisplayName
		}

		f.addLog("Raid detected: %s -> %s", sourceName, data.TargetDisplayName)

		// v1.7.0: raid handling no longer triggers immediate failover — the next
		// selector cycle (≤5 min) will repick if the source channel has gone
		// offline or changed game. The raid event itself is still processed for
		// auto-join below.
		_ = ok

		go func() {
			if err := f.gql.JoinRaid(data.RaidID); err != nil {
				f.addLog("Failed to join raid to %s: %v", data.TargetDisplayName, err)
			} else {
				f.addLog("Joined raid to %s!", data.TargetDisplayName)
			}
		}()

	case twitch.EventViewCount:
		data := evt.Data.(twitch.ViewCountData)
		if ok {
			ch.SetViewerCount(data.Viewers)
		}

	case twitch.EventError:
		if err, ok := evt.Data.(error); ok {
			f.addLog("[PubSub] %v", err)
		}

	case twitch.EventDropProgress:
		data := evt.Data.(twitch.DropProgressData)
		f.applyDropProgressUpdate(data)

	case twitch.EventDropClaim:
		data := evt.Data.(twitch.DropClaimData)
		if data.DropInstanceID != "" {
			instanceID := data.DropInstanceID
			go func() {
				if err := f.gql.ClaimDrop(instanceID); err != nil {
					f.addLog("[Drops/WS] Failed to claim drop: %v", err)
				} else {
					f.addLog("[Drops/WS] Claimed drop instance %s", instanceID)
				}
			}()
		}
		// Out-of-cycle re-run so completion + queue advance happen instantly.
		go f.processDrops()

	case twitch.EventGameChange:
		data := evt.Data.(twitch.GameChangeData)
		f.handleChannelGameChange(evt.ChannelID, data)
	}
}

// fillSpadeSlots tries to fill open Spade slots with online channels.
func (f *Farmer) fillSpadeSlots() {
	f.mu.RLock()
	var candidates []*ChannelState
	for _, ch := range f.channels {
		snap := ch.Snapshot()
		if snap.IsOnline && !snap.IsWatching {
			candidates = append(candidates, ch)
		}
	}
	f.mu.RUnlock()

	// Sort by viewer count descending (prioritize popular channels)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Snapshot().ViewerCount > candidates[j].Snapshot().ViewerCount
	})

	for _, ch := range candidates {
		if f.spade.ActiveSlots() <= 0 {
			break
		}
		f.tryStartWatching(ch)
	}
}

func (f *Farmer) balanceRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.refreshBalances()
		case <-f.stopCh:
			return
		}
	}
}

func (f *Farmer) refreshBalances() {
	f.mu.RLock()
	channels := make([]*ChannelState, 0, len(f.channels))
	for _, ch := range f.channels {
		channels = append(channels, ch)
	}
	f.mu.RUnlock()

	for _, ch := range channels {
		balance, err := f.gql.GetChannelPointsBalance(ch.Login)
		if err == nil && balance > 0 {
			ch.SetBalance(balance)
		}

		// Refresh stream info for online channels (game category, viewers, broadcast ID)
		snap := ch.Snapshot()
		if snap.IsOnline {
			info, err := f.gql.GetChannelInfo(ch.Login)
			if err == nil && info.IsLive {
				ch.SetOnline(info.BroadcastID, info.GameName, info.ViewerCount)
			}
		}

		// Small delay between API calls
		time.Sleep(500 * time.Millisecond)
	}
}

// rotationLoop rotates which 2 channels are actively watched every 5 minutes.
// Twitch only credits watch points for 2 channels at a time, so we cycle through
// all online channels to give each one watch time.
func (f *Farmer) rotationLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.rotateChannels()
		case <-f.stopCh:
			return
		}
	}
}

func (f *Farmer) rotateChannels() {
	f.mu.RLock()
	var priority0 []*ChannelState // P0: active drop (auto-promoted)
	var priority1 []*ChannelState
	var priority2 []*ChannelState
	for _, ch := range f.channels {
		snap := ch.Snapshot()
		if !snap.IsOnline {
			continue
		}
		if snap.HasActiveDrop {
			priority0 = append(priority0, ch)
		} else if snap.Priority == 1 {
			priority1 = append(priority1, ch)
		} else {
			priority2 = append(priority2, ch)
		}
	}
	f.mu.RUnlock()

	// Sort P0 by campaign end time (soonest expiring first gets the Spade slot)
	f.drops.mu.RLock()
	sort.Slice(priority0, func(i, j int) bool {
		ci := priority0[i].Snapshot().CampaignID
		cj := priority0[j].Snapshot().CampaignID
		ei := f.drops.campaignCache[ci].EndAt
		ej := f.drops.campaignCache[cj].EndAt
		if ei.IsZero() {
			return false
		}
		if ej.IsZero() {
			return true
		}
		if ei.Equal(ej) {
			return priority0[i].ChannelID < priority0[j].ChannelID
		}
		return ei.Before(ej)
	})
	f.drops.mu.RUnlock()
	sort.Slice(priority1, func(i, j int) bool {
		return priority1[i].ChannelID < priority1[j].ChannelID
	})
	sort.Slice(priority2, func(i, j int) bool {
		return priority2[i].ChannelID < priority2[j].ChannelID
	})

	// Build the desired set of channels to watch
	// P0 (drop active) → P1 (always watch) → P2 (rotate)
	const maxSlots = 2
	desired := make(map[string]*ChannelState) // channelID -> state

	slotsUsed := 0
	for _, ch := range priority0 {
		if slotsUsed >= maxSlots {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	for _, ch := range priority1 {
		if slotsUsed >= maxSlots {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	remainingSlots := maxSlots - slotsUsed
	if remainingSlots > 0 && len(priority2) > 0 {
		f.mu.Lock()
		idx := f.rotationIndex % len(priority2)
		f.rotationIndex = (f.rotationIndex + remainingSlots) % len(priority2)
		f.mu.Unlock()

		for i := 0; i < remainingSlots && i < len(priority2); i++ {
			ch := priority2[(idx+i)%len(priority2)]
			desired[ch.ChannelID] = ch
		}
	}

	// Compute diff: stop channels no longer desired, keep channels that stay
	currentlyWatching := make(map[string]bool)
	for _, list := range [][]*ChannelState{priority0, priority1, priority2} {
		for _, ch := range list {
			if ch.Snapshot().IsWatching {
				currentlyWatching[ch.ChannelID] = true
				if _, keep := desired[ch.ChannelID]; !keep {
					// Channel should stop watching
					f.spade.StopWatching(ch.ChannelID)
					ch.SetWatching(false)
				} else {
					// Channel stays — update broadcast ID in case it changed
					snap := ch.Snapshot()
					f.spade.UpdateBroadcastID(snap.ChannelID, snap.BroadcastID)
				}
			}
		}
	}

	// Start channels that are newly desired (not currently watching)
	for chID, ch := range desired {
		if currentlyWatching[chID] {
			continue // Already watching, kept running
		}
		snap := ch.Snapshot()
		// Ensure broadcast ID is set — fetch from GQL if empty
		broadcastID := snap.BroadcastID
		if broadcastID == "" {
			go f.fetchAndStartWatching(ch)
			continue
		}
		if f.spade.StartWatching(snap.ChannelID, snap.Login, broadcastID) {
			ch.SetWatching(true)
			f.addLog("Started watching %s (broadcast=%s, via rotation)", snap.DisplayName, broadcastID)
		} else {
			f.addLog("[Spade] StartWatching for %s returned false (capacity full)", snap.DisplayName)
		}
	}
}

// fetchAndStartWatching fetches broadcast ID via GQL and starts Spade for a channel.
func (f *Farmer) fetchAndStartWatching(ch *ChannelState) {
	info, err := f.gql.GetChannelInfo(ch.Login)
	if err != nil {
		f.addLog("[Spade] failed to fetch broadcast ID for %s: %v", ch.DisplayName, err)
		return
	}
	if info.BroadcastID == "" {
		f.addLog("[Spade] %s has empty broadcast ID, skipping", ch.DisplayName)
		return
	}
	ch.SetOnline(info.BroadcastID, info.GameName, info.ViewerCount)
	if f.spade.StartWatching(ch.ChannelID, ch.Login, info.BroadcastID) {
		ch.SetWatching(true)
		f.addLog("Started watching %s (broadcast=%s)", ch.DisplayName, info.BroadcastID)
	}
}

// Config returns the farmer's configuration. Used by the web layer for pin/disable mutations.
func (f *Farmer) Config() *config.Config {
	return f.cfg
}

func (f *Farmer) addLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()

	entry := LogEntry{
		Time:    now,
		Message: msg,
	}

	f.logMu.Lock()
	f.logEntries = append(f.logEntries, entry)
	// Keep last 500 entries for TUI
	if len(f.logEntries) > 500 {
		f.logEntries = f.logEntries[len(f.logEntries)-500:]
	}
	f.logMu.Unlock()

	// Write full untruncated line to debug.log
	f.writeLogFile(msg)
}

func (f *Farmer) writeLogFile(msg string) {
	if f.logFile == nil {
		return
	}

	// Daily rotation: check if we've crossed midnight
	today := time.Now().Format("2006-01-02")
	if today != f.logDate {
		newPath := fmt.Sprintf("logs/debug-%s.log", today)
		newFile, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			f.logFile.Close()
			f.logFile = newFile
			f.logDate = today
		}
	}

	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	f.logFile.WriteString(line)
}

// resolveChannelName looks up a channel name by ID for untracked channels.
// Uses a simple cache to avoid repeated GQL calls.
func (f *Farmer) resolveChannelName(channelID string) string {
	f.mu.RLock()
	if name, ok := f.nameCache[channelID]; ok {
		f.mu.RUnlock()
		return name
	}
	f.mu.RUnlock()

	// GQL lookup by channel ID
	name, err := f.gql.GetChannelNameByID(channelID)
	if err != nil || name == "" {
		return channelID // fallback to raw ID
	}

	f.mu.Lock()
	f.nameCache[channelID] = name
	f.mu.Unlock()

	return name
}

// GetUser returns the authenticated user info.
func (f *Farmer) GetUser() *twitch.UserInfo {
	return f.user
}

// GetChannels returns snapshots of all channel states.
func (f *Farmer) GetChannels() []ChannelSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()

	snapshots := make([]ChannelSnapshot, 0, len(f.channels))
	for _, ch := range f.channels {
		snapshots = append(snapshots, ch.Snapshot())
	}

	// Sort: watching first, then online, then offline — each group alphabetically
	sort.Slice(snapshots, func(i, j int) bool {
		si, sj := snapshots[i], snapshots[j]
		// Rank: 0 = watching (highest), 1 = online, 2 = offline
		rank := func(s ChannelSnapshot) int {
			if s.IsWatching {
				return 0
			}
			if s.IsOnline {
				return 1
			}
			return 2
		}
		ri, rj := rank(si), rank(sj)
		if ri != rj {
			return ri < rj
		}
		return si.DisplayName < sj.DisplayName
	})

	return snapshots
}

// GetLogs returns the recent log entries.
func (f *Farmer) GetLogs() []LogEntry {
	f.logMu.RLock()
	defer f.logMu.RUnlock()

	logs := make([]LogEntry, len(f.logEntries))
	copy(logs, f.logEntries)
	return logs
}

// GetStats returns aggregate stats.
type Stats struct {
	TotalPointsEarned int
	TotalClaimsMade   int
	Uptime            time.Duration
	ChannelsOnline    int
	ChannelsWatching  int
	ChannelsTotal     int
	ActiveDrops       int
}

func (f *Farmer) GetStats() Stats {
	f.mu.RLock()
	defer f.mu.RUnlock()

	stats := Stats{
		TotalPointsEarned: f.totalPointsEarned,
		TotalClaimsMade:   f.totalClaimsMade,
		Uptime:            time.Since(f.startTime),
		ChannelsTotal:     len(f.channels),
	}

	for _, ch := range f.channels {
		snap := ch.Snapshot()
		if snap.IsOnline {
			stats.ChannelsOnline++
		}
		if snap.IsWatching {
			stats.ChannelsWatching++
		}
	}

	f.drops.mu.RLock()
	stats.ActiveDrops = len(f.drops.activeDrops)
	f.drops.mu.RUnlock()

	return stats
}
