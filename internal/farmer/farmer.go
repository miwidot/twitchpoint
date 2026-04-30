package farmer

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/points"
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
	gql        *twitch.GQLClient
	pubsub     *twitch.PubSubClient
	spade      *twitch.SpadeTracker
	prober     *twitch.StreamProber
	dropWatch  *drops.Watcher
	dropProgC  chan drops.ProgressUpdate
	irc        *twitch.IRCClient
	events     chan twitch.FarmerEvent

	user *twitch.UserInfo

	channels *channels.Registry

	logMu      sync.RWMutex
	logEntries []LogEntry
	logFile    *os.File
	logDate    string // current log file date (YYYY-MM-DD) for rotation

	startTime time.Time
	stopCh    chan struct{}
	// stopped is atomic so Stop() doesn't need a mutex — Farmer no longer
	// owns any other shared mutable state since Phase 4 moved everything
	// across to channels.Registry / drops.Service / points.Service.
	stopped atomic.Bool

	// Drops
	drops *drops.Service

	// Points / rotation / channel-points event domain.
	// Phase 4 of the v2.0 split is in progress — Service is wired in but
	// most of the rotation/event/balance logic still lives on Farmer
	// until it gets moved across batch by batch.
	points *points.Service

	// Update checker
	update updateState
}

// New creates a new Farmer from config.
func New(cfg *config.Config, version string) *Farmer {
	return &Farmer{
		cfg:      cfg,
		version:  version,
		events:   make(chan twitch.FarmerEvent, 100),
		channels: channels.New(),
		stopCh:   make(chan struct{}),
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

	// Validate auth token by getting user info
	user, err := f.gql.GetUserInfo()
	if err != nil {
		return fmt.Errorf("auth validation failed: %w", err)
	}
	f.user = user
	f.addLog("Logged in as %s (ID: %s)", user.DisplayName, user.ID)

	// Initialize Spade tracker
	f.spade = twitch.NewSpadeTracker(user.ID, f.cfg.AuthToken, f.gql.DeviceID(), f.gql, f.addLog)
	if err := f.spade.Start(); err != nil {
		f.addLog("Spade initialization warning: %v", err)
	}

	// Initialize stream prober — fetches m3u8+chunk for picked channels so
	// drop-credit anti-cheat sees us as a real viewer (not just heartbeats).
	f.prober = twitch.NewStreamProber(f.gql, f.cfg.AuthToken, user.ID, f.gql.DeviceID(), f.debugLog)

	// Initialize drops Watcher (TDM-style single-channel watch loop).
	// Owns the picked drop channel exclusively — Spade tracker and rotation
	// must skip whatever channel ID Watcher reports as current.
	f.dropProgC = make(chan drops.ProgressUpdate, 16)
	f.dropWatch = drops.NewWatcher(f.gql, user.ID, f.dropProgC, f.debugLog)
	go f.dropProgressLoop()

	// Initialize PubSub
	f.pubsub = twitch.NewPubSubClient(f.cfg.AuthToken, f.events)

	// Initialize drops Service now that all of its deps exist (gql, spade,
	// prober, pubsub, watcher, channels registry already populated, log).
	f.drops = drops.NewService(drops.ServiceDeps{
		Cfg:                    f.cfg,
		GQL:                    f.gql,
		PubSub:                 f.pubsub,
		Spade:                  f.spade,
		Prober:                 f.prober,
		Channels:               f.channels,
		Watcher:                f.dropWatch,
		Log:                    f.addLog,
		WriteLogFile:           f.writeLogFile,
		RemoveTempChannel:      f.removeTemporaryChannel,
		AddTempChannelFromInfo: f.addTemporaryChannelFromInfo,
		// Closure binds late — f.points is constructed AFTER drops, so we
		// can't pass f.points.Rotate directly here (it would capture nil).
		TriggerRotation: func() { f.points.Rotate() },
	})

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

	// Initialize points Service AFTER IRC so we can hand it the (possibly
	// nil) IRC client. Created after drops so it can hold a reference to
	// drops.Service for cross-domain lookups (CampaignEndAt for rotation
	// sort, IsCurrentPick for offline handling). Phase 4 will move methods
	// one batch at a time; until then this Service holds state and
	// dependencies but the logic still runs from Farmer methods.
	f.points = points.NewService(points.ServiceDeps{
		Cfg:       f.cfg,
		GQL:       f.gql,
		Spade:     f.spade,
		Prober:    f.prober,
		IRC:       f.irc,
		Channels:  f.channels,
		Drops:     f.drops,
		DropWatch: f.dropWatch,
		Log:       f.addLog,
		DebugLog:  f.debugLog,
	})

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
	go f.points.BalanceRefreshLoop(f.stopCh)

	// Start channel rotation (Twitch only credits points for 2 channels at a time)
	go f.points.RotationLoop(f.stopCh)

	// Start drop mining if enabled
	if f.cfg.DropsEnabled {
		f.addLog("Drop mining enabled — checking inventory every 15 min + DropCurrentSession poll every 60s")
		go f.drops.CheckLoop(f.stopCh)
		go f.drops.ProgressPollLoop(f.stopCh)
	}

	// Start background update checker
	go f.updateCheckLoop()

	return nil
}

// Stop shuts down the farmer. Idempotent — calling twice is a no-op.
func (f *Farmer) Stop() {
	if !f.stopped.CompareAndSwap(false, true) {
		return
	}
	close(f.stopCh)

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
	if f.prober != nil {
		f.prober.StopAll()
	}
	if f.dropWatch != nil {
		f.dropWatch.StopAll()
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
		// No ID stored — resolve by login and persist the ID. This is the
		// path that needs Twitch to still know the login; once we capture
		// the ID, future startups become rename-resilient via the branch
		// above.
		info, err = f.gql.GetChannelInfo(entry.Login)
		if err == nil {
			f.cfg.SetChannelID(entry.Login, info.ID)
			f.cfg.Save()
		}
	}

	if err != nil {
		if entry.ID == "" {
			return fmt.Errorf("channel %q not found on Twitch and no ID stored to recover from a rename — remove via `--remove-channel %s`: %w",
				entry.Login, entry.Login, err)
		}
		return fmt.Errorf("get channel info: %w", err)
	}

	return f.addChannelWithInfo(info)
}

func (f *Farmer) addChannelWithInfo(info *twitch.ChannelInfo) error {
	state := channels.NewState(info.Login, info.DisplayName, info.ID)
	state.Priority = f.cfg.GetPriority(info.Login)

	f.channels.Add(state)

	// Subscribe to PubSub topics for this channel
	topics := []string{
		fmt.Sprintf("video-playback-by-id.%s", info.ID),
		fmt.Sprintf("raid.%s", info.ID),
	}
	if err := f.pubsub.Listen(topics); err != nil {
		f.addLog("PubSub subscribe error for %s: %v", info.Login, err)
	}

	f.points.NotifyChannelAdded(info.Login)

	priLabel := "rotate"
	if state.Priority == 1 {
		priLabel = "PRIORITY"
	}
	f.addLog("Added channel: %s (ID: %s) [%s]", info.DisplayName, info.ID, priLabel)

	// Check if live and start watching
	if info.IsLive {
		state.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
		f.addLog("%s is LIVE - %s (%d viewers)", info.DisplayName, info.GameName, info.ViewerCount)
		f.points.TryStartWatching(state)
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
	if ch, ok := f.channels.GetByLogin(login); ok {
		// If it's already a permanent channel, just set the campaign ID
		if !ch.Snapshot().IsTemporary {
			ch.SetCampaignID(campaignID)
			return nil
		}
		return fmt.Errorf("channel %s already tracked", login)
	}

	info, err := f.gql.GetChannelInfo(login)
	if err != nil {
		return fmt.Errorf("get channel info: %w", err)
	}

	if !info.IsLive {
		return fmt.Errorf("channel %s is not live", login)
	}

	return f.addTemporaryChannelFromInfo(info, campaignID)
}

// addTemporaryChannelFromInfo registers a temp drop channel using already-
// fetched ChannelInfo. Used by applySelectorPick which does its own
// GetChannelInfo upfront so it can validate metadata before any state
// mutation. Avoids duplicate GQL calls and ensures the temp channel is
// only registered when the metadata is provably valid.
func (f *Farmer) addTemporaryChannelFromInfo(info *twitch.ChannelInfo, campaignID string) error {
	state := channels.NewState(info.Login, info.DisplayName, info.ID)
	state.Priority = 2 // temp channels use P2 (drops will promote to P0)
	state.IsTemporary = true
	state.CampaignID = campaignID

	f.channels.Add(state)

	// Subscribe to PubSub topics
	topics := []string{
		fmt.Sprintf("video-playback-by-id.%s", info.ID),
		fmt.Sprintf("raid.%s", info.ID),
	}
	if err := f.pubsub.Listen(topics); err != nil {
		f.addLog("[Drops] PubSub subscribe error for temp channel %s: %v", info.Login, err)
	}

	f.points.NotifyChannelAdded(info.Login)

	state.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
	f.addLog("[Drops] Auto-added temporary channel: %s (campaign: %s)", info.DisplayName, campaignID)
	// FIX #3: do NOT start Spade for temp drop channels — applySelectorPick
	// (the caller of addTemporaryChannel) hands the channel directly to the
	// drops Watcher which manages it exclusively. Calling tryStartWatching
	// here would briefly start Spade only to have applySelectorPick stop it
	// 1 ms later, wasting an HTTP request and creating cross-talk.

	return nil
}

// removeTemporaryChannel cleans up a temporary channel without touching config.
func (f *Farmer) removeTemporaryChannel(channelID string) {
	ch, ok := f.channels.Remove(channelID)
	if !ok {
		return
	}
	login := ch.Login
	displayName := ch.DisplayName

	f.spade.StopWatching(channelID)
	f.prober.Stop(login)

	f.pubsub.Unlisten([]string{
		fmt.Sprintf("video-playback-by-id.%s", channelID),
		fmt.Sprintf("raid.%s", channelID),
	})

	f.points.NotifyChannelRemoved(login)

	f.addLog("[Drops] Removed temporary channel: %s", displayName)
}

// AddChannelLive adds a channel at runtime.
func (f *Farmer) AddChannelLive(login string) error {
	login = strings.ToLower(login)

	if ch, ok := f.channels.GetByLogin(login); ok {
		// If channel exists as temporary, promote to permanent
		if ch.Snapshot().IsTemporary {
			ch.SetIsTemporary(false)
			f.cfg.AddChannel(login)
			f.cfg.SetChannelID(login, ch.ChannelID)
			if err := f.cfg.Save(); err != nil {
				f.addLog("Warning: could not save config: %v", err)
			}
			f.addLog("Promoted temporary channel %s to permanent", ch.DisplayName)
			return nil
		}
		return fmt.Errorf("channel %s already added", login)
	}

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

	ch, ok := f.channels.GetByLogin(login)
	if !ok {
		return fmt.Errorf("channel %s not found", login)
	}
	channelID := ch.ChannelID

	// Temporary channels use separate cleanup (no config changes)
	if ch.Snapshot().IsTemporary {
		f.removeTemporaryChannel(channelID)
		return nil
	}

	f.channels.Remove(channelID)

	// Stop watching
	f.spade.StopWatching(channelID)
	f.prober.Stop(login)

	// Unsubscribe PubSub
	f.pubsub.Unlisten([]string{
		fmt.Sprintf("video-playback-by-id.%s", channelID),
		fmt.Sprintf("raid.%s", channelID),
	})

	f.points.NotifyChannelRemoved(login)

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
	ch, ok := f.channels.GetByLogin(login)
	if !ok {
		return fmt.Errorf("channel %s not found", login)
	}

	ch.SetPriority(priority)

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
	go f.points.Rotate()

	return nil
}

// dropProgressLoop drains drops.Watcher progress events and forwards
// them to drops.Service.ApplyProgressUpdate (which knows how to resolve
// the drop_id back to a campaign and update the channel state). This
// loop stays in farmer because it owns the drops.Watcher progress
// channel — service is the consumer.
func (f *Farmer) dropProgressLoop() {
	for {
		select {
		case ev := <-f.dropProgC:
			// ApplyProgressUpdate wants (campaign_id, drop_id) — resolve via
			// the cached inventory.
			campID := f.drops.LookupCampaignByDropID(ev.DropID)
			if campID == "" {
				continue // Unknown drop — fresh inventory cycle will catch it
			}
			f.drops.ApplyProgressUpdate(twitch.DropProgressData{
				CampaignID:             campID,
				DropID:                 ev.DropID,
				CurrentMinutesWatched:  ev.CurrentMin,
				RequiredMinutesWatched: ev.RequiredMin,
			})
		case <-f.stopCh:
			return
		}
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
	ch, ok := f.channels.Get(evt.ChannelID)

	switch evt.Type {
	case twitch.EventClaimAvailable:
		data := evt.Data.(twitch.ClaimData)

		// Dedup — only attempt each claim once.
		if f.points.SeenClaim(data.ClaimID) {
			return
		}

		channelName := evt.ChannelID
		if ok {
			channelName = ch.DisplayName
		} else {
			channelName = f.points.ResolveChannelName(evt.ChannelID)
		}

		f.points.AttemptClaim(evt.ChannelID, data.ClaimID, channelName, ch)

	case twitch.EventPointsEarned:
		data := evt.Data.(twitch.PointsData)
		f.points.RecordPoints(data.PointsGained)
		if ok {
			ch.AddPointsEarned(data.PointsGained, data.TotalPoints)
			f.addLog("+%d points on %s (%s) - Balance: %d",
				data.PointsGained, ch.DisplayName, data.ReasonCode, data.TotalPoints)
		} else {
			channelName := f.points.ResolveChannelName(evt.ChannelID)
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
					ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
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
				f.points.TryStartWatching(ch)
			}()
		}

	case twitch.EventStreamDown:
		if ok {
			snap := ch.Snapshot()
			hasDropBefore := snap.HasActiveDrop

			ch.SetOffline()
			f.spade.StopWatching(ch.ChannelID)
			f.prober.Stop(ch.Login)
			f.addLog("%s went OFFLINE", ch.DisplayName)

			// v1.8.0 (per spec section 2): if the picked drop channel just went
			// offline, trigger an out-of-cycle processDrops so the selector
			// picks a new drops-enabled channel within seconds instead of
			// waiting up to 15 minutes for the next inventory cycle.
			// Non-pick channels go through the normal slot-fill path only.
			if hasDropBefore {
				isCurrentPick := f.drops.IsCurrentPick(ch.ChannelID)
				// FIX: stop the drops Watcher RIGHT NOW for the pick — don't wait
				// for processDrops to finish (which may hang on a slow Inventory
				// fetch). Otherwise the Watcher keeps sending sendSpadeEvents
				// for an offline broadcast for 5-30s, which Twitch interprets
				// as suspicious activity.
				if isCurrentPick && f.dropWatch != nil {
					f.dropWatch.Stop()
				}
				if isCurrentPick {
					go f.drops.ProcessDrops()
				}
			}

			// Try to fill freed Spade slot
			f.points.FillSpadeSlots()
		}

	case twitch.EventRaid:
		data := evt.Data.(twitch.RaidData)

		// Dedup — PubSub fires EventRaid every second during the countdown.
		if f.points.SeenRaid(data.RaidID) {
			return
		}

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
		f.drops.ApplyProgressUpdate(data)

	case twitch.EventDropClaim:
		// Per TDM message_handlers.py:201-237: drop-claim is sequential, not
		// fire-and-forget. Steps: claim → wait 4s → poll CurrentDrop until
		// drop_id changes (max 8 retries × 2s) → re-evaluate. Doing claim and
		// processDrops as parallel goroutines (the v1.8.0-as-shipped behavior)
		// races: processDrops can pull inventory before the claim is
		// recorded, then sees the drop as still unclaimed.
		data := evt.Data.(twitch.DropClaimData)
		go f.drops.HandleDropClaim(data)

	case twitch.EventGameChange:
		data := evt.Data.(twitch.GameChangeData)
		f.drops.HandleGameChange(evt.ChannelID, data)
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

// GetUser returns the authenticated user info.
func (f *Farmer) GetUser() *twitch.UserInfo {
	return f.user
}

// GetChannels returns snapshots of all channel states.
func (f *Farmer) GetChannels() []channels.Snapshot {
	snapshots := f.channels.Snapshots()

	// Sort: watching first, then online, then offline — each group alphabetically
	sort.Slice(snapshots, func(i, j int) bool {
		si, sj := snapshots[i], snapshots[j]
		// Rank: 0 = watching (highest), 1 = online, 2 = offline
		rank := func(s channels.Snapshot) int {
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
	stats := Stats{
		TotalPointsEarned: f.points.TotalPointsEarned(),
		TotalClaimsMade:   f.points.TotalClaimsMade(),
		Uptime:            time.Since(f.startTime),
	}

	stats.ChannelsTotal = f.channels.Len()
	for _, snap := range f.channels.Snapshots() {
		if snap.IsOnline {
			stats.ChannelsOnline++
		}
		if snap.IsWatching {
			stats.ChannelsWatching++
		}
	}

	stats.ActiveDrops = f.drops.ActiveDropsCount()

	return stats
}
