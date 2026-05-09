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
