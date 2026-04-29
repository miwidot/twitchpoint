// Package drops owns the drop-mining domain: channel watch loop, drop
// progress polling, drop claiming. Cleanly separated from the points-mining
// rotation logic in farmer/ so the two domains can't interfere with each
// other (the v1.x cross-talk caused dropped drop-credit on stricter
// campaigns like ABI Partner-Only).
//
// Architecture mirrors TwitchDropsMiner (DevilXD/rangermix) services pattern:
// a single Watcher manages exactly one channel at a time, sending the
// sendSpadeEvents GQL mutation every ~59s and polling DropCurrentSession
// at the 20s mark when the in-memory progress timer indicates a minute is
// almost done. See TDM src/services/watch_service.py for the reference.
package drops

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

const (
	// WatchInterval matches TDM's WATCH_INTERVAL (59s). Twitch's drop credit
	// pipeline expects roughly one heartbeat per minute; deviating wider
	// gets the channel demoted by the credit subsystem.
	WatchInterval = 59 * time.Second
	// PollGap is the wait between sending the watch mutation and querying
	// DropCurrentSession via GQL. Mirrors TDM's `await asyncio.sleep(20)`.
	PollGap = 20 * time.Second
)

// ProgressUpdate is emitted every time the watcher sees fresh drop progress.
// channelID/login describe which pick produced the update so the consumer
// can attribute it correctly even when picks rotate.
type ProgressUpdate struct {
	ChannelID    string
	ChannelLogin string
	DropID       string
	CurrentMin   int
	RequiredMin  int
}

// gqlBackend is the minimal Twitch GQL surface the Watcher uses. Defined as
// an interface so unit tests can inject a fake without spinning up real HTTP.
type gqlBackend interface {
	SendMinuteWatched(channelID, channelLogin, broadcastID, gameName, gameID, userID string) error
	GetCurrentDropSession(channelID string) (*twitch.CurrentDropSession, error)
}

// Watcher runs the TDM-style watch loop for a single picked channel. Only
// one watch session is active at a time per Watcher — Start replaces the
// previous channel atomically.
type Watcher struct {
	gql       gqlBackend
	userID    string
	logFunc   func(string, ...any)
	progressC chan<- ProgressUpdate

	mu     sync.Mutex
	cur    *watchSession
	stopAt atomic.Bool
}

type watchSession struct {
	channelID    string
	channelLogin string
	broadcastID  string
	gameName     string
	gameID       string
	cancel       context.CancelFunc
	done         chan struct{}
}

// NewWatcher constructs a drops Watcher.
//
//   - gql: shared Twitch GQL client (must already be authenticated)
//   - userID: the logged-in Twitch user ID (sent in the spade payload)
//   - progressC: channel for ProgressUpdate events. May be nil to disable.
//   - logFunc: structured log sink. May be nil.
func NewWatcher(gql gqlBackend, userID string, progressC chan<- ProgressUpdate, logFunc func(string, ...any)) *Watcher {
	return &Watcher{
		gql:       gql,
		userID:    userID,
		logFunc:   logFunc,
		progressC: progressC,
	}
}

// Start begins watching the given channel. Any previous watch session is
// cancelled first. Safe to call concurrently — the swap is atomic.
//
// The watch loop runs in a background goroutine until either Stop is
// called or the channel goes offline (NULL DropCurrentSession after the
// first poll, kept tracking via subsequent polls).
func (w *Watcher) Start(channelID, channelLogin, broadcastID, gameName, gameID string) {
	if w.stopAt.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := &watchSession{
		channelID:    channelID,
		channelLogin: channelLogin,
		broadcastID:  broadcastID,
		gameName:     gameName,
		gameID:       gameID,
		cancel:       cancel,
		done:         make(chan struct{}),
	}

	w.mu.Lock()
	old := w.cur
	w.cur = sess
	w.mu.Unlock()

	if old != nil {
		old.cancel()
		<-old.done
	}

	go w.runLoop(ctx, sess)
}

// Stop ends the current watch session if any. Idempotent.
func (w *Watcher) Stop() {
	w.mu.Lock()
	cur := w.cur
	w.cur = nil
	w.mu.Unlock()
	if cur != nil {
		cur.cancel()
		<-cur.done
	}
}

// StopAll stops the current session and prevents any future Start.
func (w *Watcher) StopAll() {
	w.stopAt.Store(true)
	w.Stop()
}

// CurrentChannelID returns the login of the channel currently being watched
// (or empty string if no active session). Useful for the rotation logic to
// know which channel must be left alone.
func (w *Watcher) CurrentChannelID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return ""
	}
	return w.cur.channelID
}

// UpdateBroadcast updates the in-flight session's broadcast ID and game
// metadata in case the streamer changes them mid-session. No-op if the
// channel ID doesn't match the active session.
func (w *Watcher) UpdateBroadcast(channelID, broadcastID, gameName, gameID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil || w.cur.channelID != channelID {
		return
	}
	w.cur.broadcastID = broadcastID
	w.cur.gameName = gameName
	w.cur.gameID = gameID
}

func (w *Watcher) runLoop(ctx context.Context, sess *watchSession) {
	defer close(sess.done)
	if w.gql == nil {
		// Test mode: no backend, just block until cancelled.
		<-ctx.Done()
		return
	}
	w.log("[Drops/Watch] %s session start", sess.channelLogin)

	for {
		// FIX (data race): snapshot session metadata under lock — UpdateBroadcast
		// writes to sess.broadcastID/gameName/gameID with the same lock held,
		// so reading them lock-free here would race against in-place stream
		// restarts that swap broadcast_id mid-session.
		w.mu.Lock()
		snapBroadcastID := sess.broadcastID
		snapGameName := sess.gameName
		snapGameID := sess.gameID
		w.mu.Unlock()

		// 1. send_watch — sendSpadeEvents GQL mutation
		err := w.gql.SendMinuteWatched(sess.channelID, sess.channelLogin, snapBroadcastID, snapGameName, snapGameID, w.userID)
		if err != nil {
			w.log("[Drops/Watch] %s send_watch failed: %v", sess.channelLogin, err)
		}
		sentAt := time.Now()

		// 2. wait PollGap, then poll DropCurrentSession
		if w.sleep(ctx, PollGap) {
			return
		}

		sessionInfo, perr := w.gql.GetCurrentDropSession(sess.channelID)
		switch {
		case perr != nil:
			w.log("[Drops/Watch] %s CurrentDrop poll failed: %v", sess.channelLogin, perr)
		case sessionInfo == nil:
			// Twitch has not (yet) initialized a drop session for this user
			// on this channel. Common for the first 1-2 cycles after a fresh
			// pick — keep sending watch events and recheck next cycle.
		default:
			if w.progressC != nil {
				select {
				case w.progressC <- ProgressUpdate{
					ChannelID:    sess.channelID,
					ChannelLogin: sess.channelLogin,
					DropID:       sessionInfo.DropID,
					CurrentMin:   sessionInfo.CurrentMinutesWatched,
					RequiredMin:  sessionInfo.RequiredMinutesWatched,
				}:
				case <-ctx.Done():
					return
				}
			}
			w.log("[Drops/Watch] %s drop=%s mins=%d/%d",
				sess.channelLogin, shortID(sessionInfo.DropID),
				sessionInfo.CurrentMinutesWatched, sessionInfo.RequiredMinutesWatched)
		}

		// 3. sleep until WatchInterval total elapsed since send_watch
		remaining := WatchInterval - time.Since(sentAt)
		if remaining > 0 {
			if w.sleep(ctx, remaining) {
				return
			}
		}
	}
}

// sleep returns true if context was cancelled, false on normal timeout.
func (w *Watcher) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

func (w *Watcher) log(format string, args ...any) {
	if w.logFunc != nil {
		w.logFunc(format, args...)
	}
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
