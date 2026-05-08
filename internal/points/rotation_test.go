package points

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
)

func TestClassifyStreakBucket_FreshUnclaimedOnline_IsCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	// Note: OnlineSince was just set to "now"; StreakClaimedAt is zero.
	if !isStreakCandidate(ch.Snapshot(), time.Now(), "" /*dropChanID*/) {
		t.Error("fresh online + unclaimed should be streak candidate")
	}
}

func TestClassifyStreakBucket_ClaimedThisStream_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	ch.MarkStreakClaimed()
	if isStreakCandidate(ch.Snapshot(), time.Now(), "") {
		t.Error("already-claimed channel should not be candidate")
	}
}

func TestClassifyStreakBucket_OldStream_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	// Simulate a stream that's been online > 30min by passing a future "now".
	now := time.Now().Add(31 * time.Minute)
	if isStreakCandidate(ch.Snapshot(), now, "") {
		t.Error("stream online >30min should not be streak candidate")
	}
}

func TestClassifyStreakBucket_Offline_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	// never set online
	if isStreakCandidate(ch.Snapshot(), time.Now(), "") {
		t.Error("offline channel should not be streak candidate")
	}
}

func TestClassifyStreakBucket_DropOwnedByDropsWatcher_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	// drops watcher owns this channel — must be skipped
	if isStreakCandidate(ch.Snapshot(), time.Now(), "111") {
		t.Error("drops-owned channel should not be streak candidate")
	}
}

func TestClassifyStreakBucket_StaleClaimFromPriorStream_IsCandidate(t *testing.T) {
	// Stream A: online at T=0, claimed at T=1ms, offline at T=2ms.
	// Stream B (restart): online at T=3ms — claim from stream A is stale.
	// Without explicit reset, the comparison StreakClaimedAt < OnlineSince
	// must classify the channel as a fresh candidate again.
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	ch.MarkStreakClaimed()
	time.Sleep(2 * time.Millisecond)
	ch.SetOffline()
	time.Sleep(2 * time.Millisecond)
	ch.SetOnline("b2", "G", 5)
	if !isStreakCandidate(ch.Snapshot(), time.Now(), "") {
		t.Error("after restart, prior-stream claim should not block new candidacy")
	}
}

func TestSortStreakCandidates_OldestOnlineSinceFirst(t *testing.T) {
	// Build three candidates with different OnlineSince values.
	// Sort and assert: index 0 has earliest (oldest) OnlineSince.
	chOld := channels.NewState("old", "Old", "1")
	chOld.SetPriority(2)
	chOld.SetOnline("b1", "G", 5)
	time.Sleep(2 * time.Millisecond)
	chMid := channels.NewState("mid", "Mid", "2")
	chMid.SetPriority(2)
	chMid.SetOnline("b2", "G", 5)
	time.Sleep(2 * time.Millisecond)
	chNew := channels.NewState("new", "New", "3")
	chNew.SetPriority(2)
	chNew.SetOnline("b3", "G", 5)

	in := []*channels.State{chNew, chOld, chMid}
	sortStreakCandidates(in)

	if in[0].ChannelID != "1" || in[1].ChannelID != "2" || in[2].ChannelID != "3" {
		t.Errorf("sort wrong: got %s,%s,%s want 1,2,3",
			in[0].ChannelID, in[1].ChannelID, in[2].ChannelID)
	}
}
