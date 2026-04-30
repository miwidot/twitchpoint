package drops

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// Service is the orchestrator for the drops domain. It owns the
// subordinate Selector and StallTracker, the cached campaign list and
// UI row slices, and the dependencies the drop flow needs to talk to
// Twitch and the rest of the farmer.
//
// All state is private. External callers (farmer.go) reach the state
// only through accessor methods — IsCurrentPick, CampaignEndAt,
// ActiveDropsCount, GetActiveDrops, GetEligibleGames. Internal Service
// methods (in process.go, apply.go, progress.go, …) take the lock
// directly via s.mu since they live in the same package.
type Service struct {
	// Dependencies (set at construction).
	cfg                    *config.Config
	gql                    *twitch.GQLClient
	pubsub                 *twitch.PubSubClient
	spade                  *twitch.SpadeTracker
	prober                 *twitch.StreamProber
	channels               *channels.Registry
	watcher                *Watcher
	log                    func(string, ...interface{}) // visible UI + file
	writeLogFile           func(string)                 // file-only noise log
	removeTempChannel      func(channelID string)
	addTempChannelFromInfo func(info *twitch.ChannelInfo, campaignID string) error
	triggerRotation        func()

	// Subordinate services (built by NewService).
	Selector *Selector
	Stall    *StallTracker

	// State (protected by mu).
	mu                 sync.RWMutex
	activeDrops        []ActiveDrop                   // status=ACTIVE/DISABLED/COMPLETED for /api/drops
	queuedDrops        []ActiveDrop                   // status=QUEUED for /api/drops
	idleDrops          []ActiveDrop                   // status=IDLE for /api/drops
	campaignCache      map[string]twitch.DropCampaign // campaignID -> campaign, rebuilt each cycle
	currentPickID      string                         // ChannelID currently assigned the drop slot, "" if none
	lastProgressUpdate time.Time                      // when applyDropProgressUpdate last fired (WS or poll)

	// processMu serializes ProcessDrops. The 15-min CheckLoop, the 60s
	// progress poller's silent-pick path, the WS claim/game-change
	// handlers, the EventStreamDown path, and the UI/Web campaign
	// toggle all call ProcessDrops — without this lock they could
	// stack up and commit stale inventory state on top of newer state.
	// rerunRequested coalesces piled-up triggers into "exactly one
	// extra pass" after the current run finishes; concurrent triggers
	// after the rerun also get coalesced.
	processMu      sync.Mutex
	rerunRequested atomic.Bool
}

// ServiceDeps bundles the external dependencies NewService needs. The
// struct keeps the constructor call from growing into a 7-positional
// argument list.
type ServiceDeps struct {
	Cfg      *config.Config
	GQL      *twitch.GQLClient
	PubSub   *twitch.PubSubClient
	Spade    *twitch.SpadeTracker
	Prober   *twitch.StreamProber
	Channels *channels.Registry
	Watcher  *Watcher
	Log      func(string, ...interface{}) // visible UI + file
	// WriteLogFile writes file-only debug entries (used for noisy events
	// that shouldn't flood the UI feed — non-pick game-change PubSub
	// notifications, every WS drop-progress event, etc.).
	WriteLogFile func(string)
	// RemoveTempChannel is the farmer's full temp-channel teardown
	// (channels.Remove + Spade.StopWatching + prober.Stop + PubSub
	// Unlisten + IRC Part). Service calls it from CleanupNonPickedTemps;
	// owning prober/irc inside Service just for this one path would
	// expand its dep surface for no benefit.
	RemoveTempChannel func(channelID string)
	// AddTempChannelFromInfo is the farmer's temp-channel registration
	// (channels.Add + PubSub Listen + IRC Join). ApplyPick calls it
	// when the picked channel isn't tracked yet.
	AddTempChannelFromInfo func(info *twitch.ChannelInfo, campaignID string) error
	// TriggerRotation kicks the farmer's points-side Spade rotation so
	// slot 1 reflects the freshly-applied drop pick. Rotation lives in
	// farmer (it's part of the channel-points domain, not drops).
	TriggerRotation func()
}

// NewService constructs a Service with its subordinate Selector and
// StallTracker pre-built.
func NewService(deps ServiceDeps) *Service {
	return &Service{
		cfg:                    deps.Cfg,
		gql:                    deps.GQL,
		pubsub:                 deps.PubSub,
		spade:                  deps.Spade,
		prober:                 deps.Prober,
		channels:               deps.Channels,
		watcher:                deps.Watcher,
		log:                    deps.Log,
		writeLogFile:           deps.WriteLogFile,
		removeTempChannel:      deps.RemoveTempChannel,
		addTempChannelFromInfo: deps.AddTempChannelFromInfo,
		triggerRotation:        deps.TriggerRotation,
		Selector:               NewSelector(deps.Cfg, deps.GQL),
		Stall:                  NewStallTracker(deps.Log),
	}
}

// IsCurrentPick reports whether the given channelID matches the
// channel currently assigned the drop slot. Callers use this to decide
// drop-pick-specific behavior (e.g. EventStreamDown should stop the
// drops Watcher only if the offline channel was the active pick).
func (s *Service) IsCurrentPick(channelID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentPickID == channelID
}

// CampaignEndAt returns the cached EndAt for the given campaign, or
// the zero time if the campaign isn't in the cache. Used by farmer's
// rotation logic to sort priority-0 channels (channels actively
// farming a drop) by soonest-expiring campaign first.
func (s *Service) CampaignEndAt(campaignID string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.campaignCache[campaignID].EndAt
}

// ActiveDropsCount returns the length of the ACTIVE+DISABLED+COMPLETED
// row slice. Used by farmer's GetStats for the dashboard counter; if
// callers need the actual rows they should use GetActiveDrops.
func (s *Service) ActiveDropsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activeDrops)
}
