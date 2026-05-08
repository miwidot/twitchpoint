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
