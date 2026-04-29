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
	cfg      *config.Config
	gql      *twitch.GQLClient
	pubsub   *twitch.PubSubClient
	spade    *twitch.SpadeTracker
	channels *channels.Registry
	watcher  *Watcher
	log      func(string, ...interface{})

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
	Channels *channels.Registry
	Watcher  *Watcher
	Log      func(string, ...interface{})
}

// NewService constructs a Service with its subordinate Selector and
// StallTracker pre-built.
func NewService(deps ServiceDeps) *Service {
	return &Service{
		cfg:      deps.Cfg,
		gql:      deps.GQL,
		pubsub:   deps.PubSub,
		spade:    deps.Spade,
		channels: deps.Channels,
		watcher:  deps.Watcher,
		log:      deps.Log,
		Selector: NewSelector(deps.Cfg, deps.GQL),
		Stall:    NewStallTracker(deps.Log),
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
