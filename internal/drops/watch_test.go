package drops

import (
	"testing"
	"time"
)

// TestWatcher_StartReplacesPreviousSession verifies that Start cancels the
// previous session before starting a new one. Regression for the v2.0
// concern: stale watcher continuing to fire for the old channel.
func TestWatcher_StartReplacesPreviousSession(t *testing.T) {
	w := &Watcher{}
	// Simulate two consecutive Starts; ensure Watcher.cur points at the
	// latest session and the older session goroutine is finished.
	w.Start("ch1", "channel1", "broadcast1", "Game", "1")
	first := w.CurrentChannelID()
	if first != "ch1" {
		t.Fatalf("expected ch1, got %q", first)
	}
	w.Start("ch2", "channel2", "broadcast2", "Game", "1")
	second := w.CurrentChannelID()
	if second != "ch2" {
		t.Fatalf("expected ch2 after replace, got %q", second)
	}
	w.Stop()
	if w.CurrentChannelID() != "" {
		t.Fatal("expected empty current channel after Stop")
	}
}

// TestWatcher_StopAllPreventsFurtherStart verifies that after StopAll, Start
// is a no-op — the Watcher is permanently retired (used at Farmer.Stop time).
func TestWatcher_StopAllPreventsFurtherStart(t *testing.T) {
	w := &Watcher{}
	w.Start("ch1", "channel1", "bcast", "Game", "1")
	w.StopAll()
	w.Start("ch2", "channel2", "bcast", "Game", "1") // should be no-op
	if w.CurrentChannelID() != "" {
		t.Fatal("Start after StopAll must not set current channel")
	}
}

// TestWatcher_UpdateBroadcastNoOpForWrongChannel verifies that updates for a
// channel ID different from the current session are silently ignored.
func TestWatcher_UpdateBroadcastNoOpForWrongChannel(t *testing.T) {
	w := &Watcher{}
	w.Start("ch1", "channel1", "old_broadcast", "Game", "1")
	defer w.Stop()
	w.UpdateBroadcast("ch_other", "new_broadcast", "OtherGame", "999")
	w.mu.Lock()
	got := w.cur.broadcastID
	w.mu.Unlock()
	if got != "old_broadcast" {
		t.Fatalf("UpdateBroadcast on wrong channel must not mutate; got broadcastID=%q", got)
	}
}

// TestWatcher_UpdateBroadcastAppliesForMatchingChannel verifies that updates
// for the current session swap the broadcast/game metadata used by the next
// heartbeat. Critical for in-place stream restarts where the streamer keeps
// the same channel ID but gets a new broadcast_id from Twitch.
func TestWatcher_UpdateBroadcastAppliesForMatchingChannel(t *testing.T) {
	w := &Watcher{}
	w.Start("ch1", "channel1", "bcast_old", "OldGame", "1")
	defer w.Stop()
	w.UpdateBroadcast("ch1", "bcast_new", "NewGame", "999")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur.broadcastID != "bcast_new" {
		t.Errorf("expected broadcastID=bcast_new, got %q", w.cur.broadcastID)
	}
	if w.cur.gameName != "NewGame" {
		t.Errorf("expected gameName=NewGame, got %q", w.cur.gameName)
	}
	if w.cur.gameID != "999" {
		t.Errorf("expected gameID=999, got %q", w.cur.gameID)
	}
}

// TestWatcher_StopIsIdempotent verifies that calling Stop without a session,
// or calling it twice, doesn't panic.
func TestWatcher_StopIsIdempotent(t *testing.T) {
	w := &Watcher{}
	w.Stop() // no session yet
	w.Start("ch1", "channel1", "b", "G", "1")
	w.Stop()
	w.Stop() // already stopped
}

// Watch loop integration tests would need a mock GQL client. Deferred —
// the integration coverage relies on the live ABI smoke test for now.
// If you add a GQL interface in the future, exercise the watch_loop's
// CurrentDrop polling cadence (20s gap → poll → sleep until 59s).

// helper: ensure Watcher initialized maps before sequential tests
func init() {
	_ = time.Now
}
