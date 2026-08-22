package channels

import (
	"testing"
	"time"
)

func TestState_StreakClaimedAt_DefaultZero(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	snap := s.Snapshot()
	if !snap.StreakClaimedAt.IsZero() {
		t.Errorf("new state StreakClaimedAt = %v, want zero", snap.StreakClaimedAt)
	}
}

func TestState_MarkStreakClaimed_SetsTimestamp(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	before := time.Now()
	s.MarkStreakClaimed()
	after := time.Now()

	snap := s.Snapshot()
	if snap.StreakClaimedAt.Before(before) || snap.StreakClaimedAt.After(after) {
		t.Errorf("MarkStreakClaimed timestamp %v outside [%v, %v]",
			snap.StreakClaimedAt, before, after)
	}
}

func TestState_MarkStreakClaimed_OnlineSinceIndependent(t *testing.T) {
	// Verifies the comparison `StreakClaimedAt.Before(OnlineSince)` works
	// across stream restarts: claimed at T=100, channel goes offline+online
	// at T=200, comparison must say "still unclaimed for new stream".
	s := NewState("alice", "Alice", "111")
	s.SetOnline("bcast1", "Game", 5)
	s.MarkStreakClaimed()
	firstClaim := s.Snapshot().StreakClaimedAt

	// Sleep ensures OnlineSince (set by the second SetOnline) is strictly
	// after firstClaim; time.Now() resolution on some platforms is ~1ms.
	time.Sleep(2 * time.Millisecond)
	s.SetOffline()
	time.Sleep(2 * time.Millisecond)
	s.SetOnline("bcast2", "Game", 5)

	snap := s.Snapshot()
	if !firstClaim.Before(snap.OnlineSince) {
		t.Errorf("after stream restart, expected old claim %v < new OnlineSince %v",
			firstClaim, snap.OnlineSince)
	}
}

func TestState_SetOnlineWithGameID_UsesStreamStartedAt(t *testing.T) {
	// When caller provides a non-zero streamStartedAt, that becomes
	// OnlineSince — replacing the time.Now() default so the streak
	// window is measured from the real stream start.
	s := NewState("alice", "Alice", "111")
	realStart := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)

	s.SetOnlineWithGameID("bcast1", "Game", "g1", 5, realStart)

	snap := s.Snapshot()
	if !snap.OnlineSince.Equal(realStart) {
		t.Errorf("OnlineSince = %v, want %v", snap.OnlineSince, realStart)
	}
}

func TestState_SetOnlineWithGameID_FallsBackToNow(t *testing.T) {
	// Zero streamStartedAt (e.g. GQL didn't return createdAt) falls
	// back to time.Now() so callers without GQL info still work.
	s := NewState("alice", "Alice", "111")
	before := time.Now()
	s.SetOnlineWithGameID("bcast1", "Game", "g1", 5, time.Time{})
	after := time.Now()

	snap := s.Snapshot()
	if snap.OnlineSince.Before(before) || snap.OnlineSince.After(after) {
		t.Errorf("OnlineSince fallback %v not in [%v, %v]",
			snap.OnlineSince, before, after)
	}
}

// TestState_StreamWatched_AccumulatesAcrossSlices: rotation swaps a
// channel out every few minutes, so the streak requirement is only ever
// met across several watch slices. Each slice must be folded into the
// per-stream total when watching stops.
func TestState_StreamWatched_AccumulatesAcrossSlices(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	s.SetOnline("b1", "Game", 5)

	for i := 0; i < 3; i++ {
		s.SetWatching(true)
		time.Sleep(3 * time.Millisecond)
		s.SetWatching(false)
	}

	if got := s.StreamWatchedFor(); got < 9*time.Millisecond {
		t.Errorf("StreamWatched = %v, want >= 9ms accumulated across 3 slices", got)
	}
}

// TestState_StreamWatched_IncludesSliceInProgress: the gate must see the
// time being watched right now, not only completed slices — otherwise a
// channel holding a slot looks permanently unwatched.
func TestState_StreamWatched_IncludesSliceInProgress(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	s.SetOnline("b1", "Game", 5)
	s.SetWatching(true)
	time.Sleep(3 * time.Millisecond)

	if got := s.StreamWatchedFor(); got < 3*time.Millisecond {
		t.Errorf("StreamWatched = %v, want the in-progress slice counted", got)
	}
	if got := s.Snapshot().StreamWatched; got < 3*time.Millisecond {
		t.Errorf("Snapshot.StreamWatched = %v, want the in-progress slice counted", got)
	}
}

// TestState_StreamWatched_ResetsOnNewBroadcast: Twitch counts the watched
// minutes per broadcast, so a new stream must start from zero — otherwise
// the previous stream's total makes the new one look already satisfied and
// its streak hunt is skipped.
func TestState_StreamWatched_ResetsOnNewBroadcast(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	s.SetOnline("b1", "Game", 5)
	s.SetWatching(true)
	time.Sleep(3 * time.Millisecond)
	s.SetWatching(false)
	if s.StreamWatchedFor() == 0 {
		t.Fatal("precondition: expected watch time on the first stream")
	}

	// Streamer restarts: new broadcast ID while still "online".
	s.SetOnline("b2", "Game", 5)
	if got := s.StreamWatchedFor(); got != 0 {
		t.Errorf("StreamWatched = %v after new broadcast, want 0", got)
	}

	// And across a proper offline → online cycle.
	s.SetWatching(true)
	time.Sleep(3 * time.Millisecond)
	s.SetOffline()
	s.SetOnline("b3", "Game", 5)
	if got := s.StreamWatchedFor(); got != 0 {
		t.Errorf("StreamWatched = %v after restart, want 0", got)
	}
}

// TestState_StreamWatched_SurvivesBroadcastRefresh: rotation and the drops
// watcher push the SAME broadcast ID repeatedly; that must not wipe the
// accumulated total, or the channel could never reach the streak target.
func TestState_StreamWatched_SurvivesBroadcastRefresh(t *testing.T) {
	s := NewState("alice", "Alice", "111")
	s.SetOnline("b1", "Game", 5)
	s.SetWatching(true)
	time.Sleep(3 * time.Millisecond)
	s.SetWatching(false)
	before := s.StreamWatchedFor()

	s.SetOnline("b1", "Game", 7) // same broadcast, refreshed viewer count
	if got := s.StreamWatchedFor(); got != before {
		t.Errorf("StreamWatched = %v after same-broadcast refresh, want %v", got, before)
	}
}
