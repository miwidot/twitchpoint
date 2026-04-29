package drops

import (
	"sync"
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
// The exported state fields (ActiveDrops, CampaignCache, CurrentPickID,
// …) and the Lock/Unlock pass-throughs are transitional: they let
// farmer keep its existing access patterns while methods migrate over
// from farmer/drops.go one phase at a time. Later phases will encapsulate
// each access behind a Service method and demote the fields to
// unexported.
type Service struct {
	// Dependencies (set at construction).
	cfg                     *config.Config
	gql                     *twitch.GQLClient
	pubsub                  *twitch.PubSubClient
	spade                   *twitch.SpadeTracker
	prober                  *twitch.StreamProber
	channels                *channels.Registry
	watcher                 *Watcher
	log                     func(string, ...interface{}) // visible UI + file
	writeLogFile            func(string)                 // file-only noise log
	removeTempChannel       func(channelID string)
	addTempChannelFromInfo  func(info *twitch.ChannelInfo, campaignID string) error
	triggerProcessDrops     func()

	// Subordinate services (built by NewService).
	Selector *Selector
	Stall    *StallTracker

	// State (protected by mu).
	mu                 sync.RWMutex
	ActiveDrops        []ActiveDrop                   // status=ACTIVE/DISABLED/COMPLETED for /api/drops
	QueuedDrops        []ActiveDrop                   // status=QUEUED for /api/drops
	IdleDrops          []ActiveDrop                   // status=IDLE for /api/drops
	CampaignCache      map[string]twitch.DropCampaign // campaignID -> campaign, rebuilt each cycle
	CurrentPickID      string                         // ChannelID currently assigned the drop slot, "" if none
	LastProgressUpdate time.Time                      // when applyDropProgressUpdate last fired (WS or poll)
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
	// TriggerProcessDrops re-runs the inventory cycle out-of-band. Used
	// by HandleGameChange after the 30s debounce when the picked
	// streamer settled on a wrong game and we need a fresh selector
	// pass. processDrops still lives in farmer until Phase 2g.
	TriggerProcessDrops func()
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
		triggerProcessDrops:    deps.TriggerProcessDrops,
		Selector:               NewSelector(deps.Cfg, deps.GQL),
		Stall:                  NewStallTracker(deps.Log),
	}
}

// Lock / Unlock / RLock / RUnlock are transitional pass-throughs to the
// state mutex. Farmer code still acquires the lock directly for compound
// reads/writes of the exported state fields. As each access pattern
// migrates into a Service method, the corresponding caller stops needing
// these — final goal is to remove them entirely.
func (s *Service) Lock()    { s.mu.Lock() }
func (s *Service) Unlock()  { s.mu.Unlock() }
func (s *Service) RLock()   { s.mu.RLock() }
func (s *Service) RUnlock() { s.mu.RUnlock() }
