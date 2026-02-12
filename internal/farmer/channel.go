package farmer

import (
	"sync"
	"time"
)

// ChannelState tracks the state of a single Twitch channel being farmed.
type ChannelState struct {
	mu sync.RWMutex

	// Identity
	Login       string
	DisplayName string
	ChannelID   string

	// Priority
	Priority    int  // 1 = always watch, 2 = rotate

	// Status
	IsOnline    bool
	IsWatching  bool // Spade heartbeat active
	BroadcastID string
	GameName    string
	ViewerCount int

	// Points
	PointsBalance      int
	PointsEarnedSession int
	ClaimsMade         int
	LastClaimTime      time.Time

	// Timing
	OnlineSince  time.Time
	WatchingSince time.Time

	// Drops
	HasActiveDrop bool
	DropName      string
	DropProgress  int // current minutes watched
	DropRequired  int // required minutes to complete
}

// NewChannelState creates a new channel state.
func NewChannelState(login, displayName, channelID string) *ChannelState {
	return &ChannelState{
		Login:       login,
		DisplayName: displayName,
		ChannelID:   channelID,
	}
}

// SetOnline marks the channel as online.
func (c *ChannelState) SetOnline(broadcastID, gameName string, viewers int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.IsOnline {
		c.OnlineSince = time.Now()
	}
	c.IsOnline = true
	c.BroadcastID = broadcastID
	c.GameName = gameName
	c.ViewerCount = viewers
}

// SetOffline marks the channel as offline.
func (c *ChannelState) SetOffline() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.IsOnline = false
	c.IsWatching = false
	c.BroadcastID = ""
	c.GameName = ""
	c.ViewerCount = 0
}

// SetWatching marks the channel as actively being watched (Spade).
func (c *ChannelState) SetWatching(watching bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.IsWatching = watching
	if watching && c.WatchingSince.IsZero() {
		c.WatchingSince = time.Now()
	}
	if !watching {
		c.WatchingSince = time.Time{}
	}
}

// AddPointsEarned records earned points.
func (c *ChannelState) AddPointsEarned(points int, totalBalance int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PointsEarnedSession += points
	if totalBalance > 0 {
		c.PointsBalance = totalBalance
	}
}

// RecordClaim records a bonus claim.
func (c *ChannelState) RecordClaim() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ClaimsMade++
	c.LastClaimTime = time.Now()
}

// SetBalance sets the points balance.
func (c *ChannelState) SetBalance(balance int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PointsBalance = balance
}

// SetViewerCount updates the viewer count.
func (c *ChannelState) SetViewerCount(count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ViewerCount = count
}

// SetDropInfo sets drop tracking fields for this channel.
func (c *ChannelState) SetDropInfo(name string, progress, required int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HasActiveDrop = true
	c.DropName = name
	c.DropProgress = progress
	c.DropRequired = required
}

// ClearDropInfo removes drop tracking from this channel.
func (c *ChannelState) ClearDropInfo() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HasActiveDrop = false
	c.DropName = ""
	c.DropProgress = 0
	c.DropRequired = 0
}

// Snapshot returns a read-only copy of the channel state.
type ChannelSnapshot struct {
	Login              string
	DisplayName        string
	ChannelID          string
	Priority           int
	IsOnline           bool
	IsWatching         bool
	BroadcastID        string
	GameName           string
	ViewerCount        int
	PointsBalance      int
	PointsEarnedSession int
	ClaimsMade         int
	LastClaimTime      time.Time
	OnlineSince        time.Time
	WatchingSince      time.Time

	// Drops
	HasActiveDrop bool
	DropName      string
	DropProgress  int
	DropRequired  int
}

// Snapshot returns a thread-safe copy of the current state.
func (c *ChannelState) Snapshot() ChannelSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return ChannelSnapshot{
		Login:               c.Login,
		DisplayName:         c.DisplayName,
		ChannelID:           c.ChannelID,
		Priority:            c.Priority,
		IsOnline:            c.IsOnline,
		IsWatching:          c.IsWatching,
		BroadcastID:         c.BroadcastID,
		GameName:            c.GameName,
		ViewerCount:         c.ViewerCount,
		PointsBalance:       c.PointsBalance,
		PointsEarnedSession: c.PointsEarnedSession,
		ClaimsMade:          c.ClaimsMade,
		LastClaimTime:       c.LastClaimTime,
		OnlineSince:         c.OnlineSince,
		WatchingSince:       c.WatchingSince,
		HasActiveDrop:       c.HasActiveDrop,
		DropName:            c.DropName,
		DropProgress:        c.DropProgress,
		DropRequired:        c.DropRequired,
	}
}
