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
	cfg    *config.Config
	gql    *twitch.GQLClient
	pubsub *twitch.PubSubClient
	spade  *twitch.SpadeTracker
	irc    *twitch.IRCClient
	events chan twitch.FarmerEvent

	user *twitch.UserInfo

	mu       sync.RWMutex
	channels map[string]*ChannelState // channelID -> state
	loginMap map[string]string        // login -> channelID

	logMu      sync.RWMutex
	logEntries []LogEntry
	logFile    *os.File

	startTime time.Time
	stopCh    chan struct{}
	stopped   bool

	// Stats
	totalPointsEarned int
	totalClaimsMade   int

	// Dedup
	seenClaims map[string]time.Time // claimID -> when we attempted
	seenRaids  map[string]time.Time // raidID -> when we attempted

	// Rotation
	rotationIndex int // which pair of channels is currently being watched
}

// New creates a new Farmer from config.
func New(cfg *config.Config) *Farmer {
	return &Farmer{
		cfg:        cfg,
		events:     make(chan twitch.FarmerEvent, 100),
		channels:   make(map[string]*ChannelState),
		loginMap:   make(map[string]string),
		seenClaims: make(map[string]time.Time),
		seenRaids:  make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
}

// Start initializes all subsystems and begins farming.
func (f *Farmer) Start() error {
	f.startTime = time.Now()

	// Open debug log file (append mode)
	logFile, err := os.OpenFile("debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open debug.log: %w", err)
	}
	f.logFile = logFile
	f.writeLogFile("=== TwitchPoint Farmer started ===")

	// Initialize GQL client
	f.gql = twitch.NewGQLClient(f.cfg.AuthToken)

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

	// Subscribe to user-level community points topic
	if err := f.pubsub.Listen([]string{
		fmt.Sprintf("community-points-user-v1.%s", user.ID),
	}); err != nil {
		f.addLog("PubSub user topic error: %v", err)
	}

	// Initialize IRC for viewer presence
	if f.cfg.IrcEnabled {
		f.irc = twitch.NewIRCClient(f.cfg.AuthToken, user.Login, f.addLog)
	}

	// Initialize channels first (stores all PubSub topics before connecting)
	for _, login := range f.cfg.GetChannelLogins() {
		if err := f.addChannel(login); err != nil {
			f.addLog("Failed to add channel %s: %v", login, err)
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

func (f *Farmer) addChannel(login string) error {
	info, err := f.gql.GetChannelInfo(login)
	if err != nil {
		return fmt.Errorf("get channel info: %w", err)
	}

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
		f.addLog("PubSub subscribe error for %s: %v", login, err)
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
	go func() {
		balance, err := f.gql.GetChannelPointsBalance(login)
		if err == nil && balance > 0 {
			state.SetBalance(balance)
			f.addLog("%s balance: %d points", info.DisplayName, balance)
		}
	}()

	return nil
}

// AddChannelLive adds a channel at runtime.
func (f *Farmer) AddChannelLive(login string) error {
	f.mu.RLock()
	for _, id := range f.loginMap {
		if ch, ok := f.channels[id]; ok && ch.Login == login {
			f.mu.RUnlock()
			return fmt.Errorf("channel %s already added", login)
		}
	}
	f.mu.RUnlock()

	if err := f.addChannel(login); err != nil {
		return err
	}

	// Also save to config
	f.cfg.AddChannel(login)
	if err := f.cfg.Save(); err != nil {
		f.addLog("Warning: could not save config: %v", err)
	}

	return nil
}

// RemoveChannelLive removes a channel at runtime.
func (f *Farmer) RemoveChannelLive(login string) error {
	f.mu.Lock()
	channelID, ok := f.loginMap[login]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("channel %s not found", login)
	}

	ch := f.channels[channelID]
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
		f.mu.RLock()
		var claimCh *ChannelState
		for _, c := range f.channels {
			if c.ChannelID == evt.ChannelID {
				claimCh = c
				channelName = c.DisplayName
				break
			}
		}
		f.mu.RUnlock()

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
		if ok {
			ch.AddPointsEarned(data.PointsGained, data.TotalPoints)
			f.mu.Lock()
			f.totalPointsEarned += data.PointsGained
			f.mu.Unlock()
			f.addLog("+%d points on %s (%s) - Balance: %d",
				data.PointsGained, ch.DisplayName, data.ReasonCode, data.TotalPoints)
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
			ch.SetOffline()
			f.spade.StopWatching(ch.ChannelID)
			f.addLog("%s went OFFLINE", ch.DisplayName)
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
	var priority1 []*ChannelState
	var priority2 []*ChannelState
	for _, ch := range f.channels {
		snap := ch.Snapshot()
		if !snap.IsOnline {
			continue
		}
		if snap.Priority == 1 {
			priority1 = append(priority1, ch)
		} else {
			priority2 = append(priority2, ch)
		}
	}
	f.mu.RUnlock()

	// Sort both lists deterministically by channel ID
	sort.Slice(priority1, func(i, j int) bool {
		return priority1[i].ChannelID < priority1[j].ChannelID
	})
	sort.Slice(priority2, func(i, j int) bool {
		return priority2[i].ChannelID < priority2[j].ChannelID
	})

	// Build the desired set of channels to watch
	const maxSlots = 2
	desired := make(map[string]*ChannelState) // channelID -> state

	slotsUsed := 0
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
	for _, list := range [][]*ChannelState{priority1, priority2} {
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
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	f.logFile.WriteString(line)
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

	// Sort by display name
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].DisplayName < snapshots[j].DisplayName
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

	return stats
}
