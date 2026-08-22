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

	// Streak-Hunt tracking. StreakClaimedAt is set when a WATCH_STREAK
	// PubSub event fires for this channel; the rotation logic compares
	// it to OnlineSince to decide if the current stream's streak is
	// still claimable. Surviving across SetOffline is intentional —
	// the comparison `StreakClaimedAt < OnlineSince` makes the new
	// stream "unclaimed" without an explicit reset.
	StreakClaimedAt time.Time

	// StreamWatched is how long we have watched the CURRENT stream in
	// total, across however many rotation slices it took. Twitch grants
	// the WATCH_STREAK bonus once a broadcast has been watched for
	// ~10 minutes; that requirement is about accumulated WATCH TIME, not
	// about how long ago the stream started, so this — not a wall-clock
	// deadline — is what decides whether a streak is still obtainable.
	// Reset when a new broadcast begins (see resetStreamWatchLocked).
	StreamWatched time.Duration

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
	s.resetStreamWatchIfNewBroadcastLocked(broadcastID)
	s.IsOnline = true
	s.BroadcastID = broadcastID
	s.GameName = gameName
	s.ViewerCount = viewers
}

// SetOnlineWithGameID is like SetOnline but also stores the game ID and
// accepts the real Twitch stream-start time (from GQL stream.createdAt).
// Used by the drops watcher payload (sendSpadeEvents requires real game_id)
// and by every code path that promotes a channel from offline to online —
// so OnlineSince reflects when the stream actually started, not when our
// bot first noticed it. A zero streamStartedAt falls back to time.Now(),
// preserving behavior for callers without GQL info (e.g. tests).
func (s *State) SetOnlineWithGameID(broadcastID, gameName, gameID string, viewers int, streamStartedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.IsOnline {
		if streamStartedAt.IsZero() {
			s.OnlineSince = time.Now()
		} else {
			s.OnlineSince = streamStartedAt
		}
	}
	s.resetStreamWatchIfNewBroadcastLocked(broadcastID)
	s.IsOnline = true
	s.BroadcastID = broadcastID
	s.GameName = gameName
	s.GameID = gameID
	s.ViewerCount = viewers
}

// resetStreamWatchIfNewBroadcastLocked clears the accumulated watch time
// when the channel starts a DIFFERENT broadcast than the one we were
// tracking — either coming back from offline, or the streamer restarting
// mid-session (new broadcast ID). Twitch counts the ~10 watched minutes
// per broadcast, so carrying the previous stream's total over would make
// the new stream look already-satisfied and skip its streak hunt.
//
// Refreshing the same broadcast ID (the common case — rotation and the
// drops watcher push it repeatedly) must NOT reset the accumulator.
// Caller holds s.mu.
func (s *State) resetStreamWatchIfNewBroadcastLocked(broadcastID string) {
	if !s.IsOnline || (broadcastID != "" && broadcastID != s.BroadcastID) {
		s.StreamWatched = 0
		s.WatchingSince = time.Time{}
	}
}

// accumulateWatchLocked folds an in-progress watch slice into StreamWatched
// and clears the running clock. Caller holds s.mu.
func (s *State) accumulateWatchLocked() {
	if s.WatchingSince.IsZero() {
		return
	}
	if d := time.Since(s.WatchingSince); d > 0 {
		s.StreamWatched += d
	}
	s.WatchingSince = time.Time{}
}

// StreamWatchedFor reports the total watch time for the current stream,
// including the slice currently in progress. This is the value the
// Streak-Hunt logic gates on.
func (s *State) StreamWatchedFor() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streamWatchedLocked()
}

// streamWatchedLocked is StreamWatchedFor without the lock. Caller holds s.mu.
func (s *State) streamWatchedLocked() time.Duration {
	total := s.StreamWatched
	if s.IsWatching && !s.WatchingSince.IsZero() {
		if d := time.Since(s.WatchingSince); d > 0 {
			total += d
		}
	}
	return total
}

// SetOffline marks the channel as offline.
func (s *State) SetOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulateWatchLocked()
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
	if !watching {
		// Fold the finished slice into the per-stream total before the
		// running clock is cleared — rotation swaps a channel out every
		// few minutes, so the streak requirement is only ever met across
		// several slices.
		s.accumulateWatchLocked()
	}
	s.IsWatching = watching
	if watching && s.WatchingSince.IsZero() {
		s.WatchingSince = time.Now()
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

// MarkStreakClaimed records that Twitch granted the WATCH_STREAK bonus
// for the current stream. Called from the farmer's PointsEarned event
// handler when the PubSub event carries a WATCH_STREAK reason code.
func (s *State) MarkStreakClaimed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.StreakClaimedAt = time.Now()
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

	// Streak-Hunt
	StreakClaimedAt time.Time
	// StreamWatched is the accumulated watch time for the current stream,
	// including any slice in progress at snapshot time.
	StreamWatched time.Duration

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
		StreakClaimedAt:     s.StreakClaimedAt,
		StreamWatched:       s.streamWatchedLocked(),
		HasActiveDrop:       s.HasActiveDrop,
		DropName:            s.DropName,
		DropProgress:        s.DropProgress,
		DropRequired:        s.DropRequired,
		IsTemporary:         s.IsTemporary,
		CampaignID:          s.CampaignID,
	}
}
