package points

import (
	"sync"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// Service is the orchestrator for the channel-points / rotation domain.
// It owns the 5-minute Spade rotation, the channel-points event handlers
// (claim, points-earned, stream up/down, raid), the periodic balance
// refresh, and the dedup/stat counters that used to live on Farmer.
//
// All state is private. External callers reach the state only through
// accessor methods (Stats, TotalPointsEarned, TotalClaimsMade,
// ResolveChannelName). Internal Service methods (in rotation.go,
// events.go, balance.go) take the lock directly via s.mu since they
// live in the same package.
//
// Phase 4 of the v2.0 split is in progress — for now the Service holds
// dependencies and state, but the actual rotation/event/balance logic
// still lives in farmer.go. Subsequent batches move them in.
type Service struct {
	// Dependencies (set at construction).
	cfg       *config.Config
	gql       *twitch.GQLClient
	spade     *twitch.SpadeTracker
	prober    *twitch.StreamProber
	irc       *twitch.IRCClient
	channels  *channels.Registry
	drops     *drops.Service
	dropWatch *drops.Watcher
	log       func(string, ...interface{}) // visible UI + file
	debugLog  func(string, ...interface{}) // file-only by default (-tags=debug surfaces in UI)

	// State (protected by mu).
	mu                sync.RWMutex
	seenClaims        map[string]time.Time // claimID -> when we attempted (dedup)
	seenRaids         map[string]time.Time // raidID -> when we attempted (dedup)
	totalPointsEarned int
	totalClaimsMade   int
	nameCache         map[string]string // channelID -> displayName, for untracked channels
	rotationIndex     int               // priority-2 channel cursor for the 5-min rotation
}

// ServiceDeps bundles the external dependencies NewService needs. Mirrors
// drops.ServiceDeps so the constructor call doesn't grow into a long
// positional argument list.
type ServiceDeps struct {
	Cfg       *config.Config
	GQL       *twitch.GQLClient
	Spade     *twitch.SpadeTracker
	Prober    *twitch.StreamProber
	IRC       *twitch.IRCClient // may be nil if IrcEnabled=false
	Channels  *channels.Registry
	Drops     *drops.Service
	DropWatch *drops.Watcher
	Log       func(string, ...interface{}) // visible UI + file
	DebugLog  func(string, ...interface{}) // file-only by default
}

// NewService constructs a Service with empty dedup/stat maps.
func NewService(deps ServiceDeps) *Service {
	return &Service{
		cfg:        deps.Cfg,
		gql:        deps.GQL,
		spade:      deps.Spade,
		prober:     deps.Prober,
		irc:        deps.IRC,
		channels:   deps.Channels,
		drops:      deps.Drops,
		dropWatch:  deps.DropWatch,
		log:        deps.Log,
		debugLog:   deps.DebugLog,
		seenClaims: make(map[string]time.Time),
		seenRaids:  make(map[string]time.Time),
		nameCache:  make(map[string]string),
	}
}

// TotalPointsEarned returns the running sum of points credited via
// PubSub PointsEarned events since farmer start.
func (s *Service) TotalPointsEarned() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalPointsEarned
}

// TotalClaimsMade returns the running count of bonus-claims successfully
// completed via ClaimCommunityPoints.
func (s *Service) TotalClaimsMade() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalClaimsMade
}
