// Package channels owns the per-Twitch-channel mutable state and the
// process-wide registry that maps it. The registry is the single source
// of truth for "is this channel tracked?" — farmer, drops, and web
// layers read from it but never own the underlying maps.
package channels

import (
	"sync"
	"time"
)

// State tracks the live state of a single Twitch channel being farmed.
// All mutations go through the methods below; direct field writes are
// only safe before the State is handed to a Registry.
type State struct {
	mu sync.RWMutex

	// Identity
	Login       string
	DisplayName string
	ChannelID   string

	// Priority
	Priority int // 1 = always watch, 2 = rotate

	// Status
	IsOnline    bool
	IsWatching  bool // Spade heartbeat active
	BroadcastID string
	GameName    string
	GameID      string
	ViewerCount int

	// Points
	PointsBalance       int
	PointsEarnedSession int
	ClaimsMade          int
	LastClaimTime       time.Time

	// Timing
	OnlineSince   time.Time
	WatchingSince time.Time

	// Drops
	HasActiveDrop bool
	DropName      string
	DropProgress  int // current minutes watched
	DropRequired  int // required minutes to complete

	// Temporary channel (auto-added for drops, not saved to config)
	IsTemporary bool
	CampaignID  string // which campaign this channel serves
}

// NewState creates a new channel state.
func NewState(login, displayName, channelID string) *State {
	return &State{
		Login:       login,
		DisplayName: displayName,
		ChannelID:   channelID,
	}
}

// SetPriority changes the rotation priority (1 = always watch, 2 = rotate).
func (s *State) SetPriority(p int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Priority = p
}

// SetIsTemporary toggles the temporary-channel flag (used when a drops-only
// channel is promoted to permanent or vice versa).
func (s *State) SetIsTemporary(t bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IsTemporary = t
}

// SetCampaignID stores which drop campaign this channel currently serves.
func (s *State) SetCampaignID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CampaignID = id
}

// SetOnline marks the channel as online.
func (s *State) SetOnline(broadcastID, gameName string, viewers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.IsOnline {
		s.OnlineSince = time.Now()
	}
	s.IsOnline = true
	s.BroadcastID = broadcastID
	s.GameName = gameName
	s.ViewerCount = viewers
}

// SetOnlineWithGameID is like SetOnline but also stores the game ID.
// Used by the drops watcher payload (sendSpadeEvents requires real game_id).
func (s *State) SetOnlineWithGameID(broadcastID, gameName, gameID string, viewers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.IsOnline {
		s.OnlineSince = time.Now()
	}
	s.IsOnline = true
	s.BroadcastID = broadcastID
	s.GameName = gameName
	s.GameID = gameID
	s.ViewerCount = viewers
}

// SetOffline marks the channel as offline.
func (s *State) SetOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IsOnline = false
	s.IsWatching = false
	s.BroadcastID = ""
	s.GameName = ""
	s.GameID = ""
	s.ViewerCount = 0
}

// SetWatching marks the channel as actively being watched (Spade).
func (s *State) SetWatching(watching bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IsWatching = watching
	if watching && s.WatchingSince.IsZero() {
		s.WatchingSince = time.Now()
	}
	if !watching {
		s.WatchingSince = time.Time{}
	}
}

// AddPointsEarned records earned points.
func (s *State) AddPointsEarned(points int, totalBalance int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PointsEarnedSession += points
	if totalBalance > 0 {
		s.PointsBalance = totalBalance
	}
}

// RecordClaim records a bonus claim.
func (s *State) RecordClaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ClaimsMade++
	s.LastClaimTime = time.Now()
}

// SetBalance sets the points balance.
func (s *State) SetBalance(balance int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PointsBalance = balance
}

// SetViewerCount updates the viewer count.
func (s *State) SetViewerCount(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ViewerCount = count
}

// SetDropInfo sets drop tracking fields for this channel.
func (s *State) SetDropInfo(name string, progress, required int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.HasActiveDrop = true
	s.DropName = name
	s.DropProgress = progress
	s.DropRequired = required
}

// ClearDropInfo removes drop tracking from this channel.
func (s *State) ClearDropInfo() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.HasActiveDrop = false
	s.DropName = ""
	s.DropProgress = 0
	s.DropRequired = 0
	s.CampaignID = ""
}

// Snapshot is an immutable read-only copy of a State.
type Snapshot struct {
	Login               string
	DisplayName         string
	ChannelID           string
	Priority            int
	IsOnline            bool
	IsWatching          bool
	BroadcastID         string
	GameName            string
	GameID              string
	ViewerCount         int
	PointsBalance       int
	PointsEarnedSession int
	ClaimsMade          int
	LastClaimTime       time.Time
	OnlineSince         time.Time
	WatchingSince       time.Time

	// Drops
	HasActiveDrop bool
	DropName      string
	DropProgress  int
	DropRequired  int

	// Temporary channel
	IsTemporary bool
	CampaignID  string
}

// Snapshot returns a thread-safe copy of the current state.
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Login:               s.Login,
		DisplayName:         s.DisplayName,
		ChannelID:           s.ChannelID,
		Priority:            s.Priority,
		IsOnline:            s.IsOnline,
		IsWatching:          s.IsWatching,
		BroadcastID:         s.BroadcastID,
		GameName:            s.GameName,
		GameID:              s.GameID,
		ViewerCount:         s.ViewerCount,
		PointsBalance:       s.PointsBalance,
		PointsEarnedSession: s.PointsEarnedSession,
		ClaimsMade:          s.ClaimsMade,
		LastClaimTime:       s.LastClaimTime,
		OnlineSince:         s.OnlineSince,
		WatchingSince:       s.WatchingSince,
		HasActiveDrop:       s.HasActiveDrop,
		DropName:            s.DropName,
		DropProgress:        s.DropProgress,
		DropRequired:        s.DropRequired,
		IsTemporary:         s.IsTemporary,
		CampaignID:          s.CampaignID,
	}
}
