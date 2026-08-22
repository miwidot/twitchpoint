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
	if !isStreakCandidate(ch.Snapshot(), "" /*dropChanID*/) {
		t.Error("fresh online + unclaimed should be streak candidate")
	}
}

func TestClassifyStreakBucket_ClaimedThisStream_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	ch.MarkStreakClaimed()
	if isStreakCandidate(ch.Snapshot(), "") {
		t.Error("already-claimed channel should not be candidate")
	}
}

// TestClassifyStreakBucket_LongRunningUnwatchedStream_IsCandidate is the
// regression guard for the streak-reset bug: a stream that has been live
// for hours but that we never got a slot for is STILL earning its streak,
// because Twitch's requirement is ~10 watched minutes, not "watched within
// the first 30 minutes". The previous wall-clock cutoff abandoned exactly
// these channels — every channel that went live while both Spade slots
// were busy — so their streak broke and reset the next stream.
func TestClassifyStreakBucket_LongRunningUnwatchedStream_IsCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	// Stream started 4 hours ago; we never watched a single minute of it.
	ch.SetOnlineWithGameID("b1", "G", "42", 5, time.Now().Add(-4*time.Hour))
	if !isStreakCandidate(ch.Snapshot(), "") {
		t.Error("long-running but unwatched stream must still be a streak candidate")
	}
}

// TestClassifyStreakBucket_AlreadyWatchedEnough_NotCandidate: once we have
// watched the stream past the target, the bonus either landed or won't
// come at all — retire the candidate so the slot serves someone else.
func TestClassifyStreakBucket_AlreadyWatchedEnough_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	ch.StreamWatched = streakWatchTarget
	if isStreakCandidate(ch.Snapshot(), "") {
		t.Error("stream already watched past the target should not be a candidate")
	}
}

// TestClassifyStreakBucket_PartiallyWatched_IsCandidate: below the target
// the streak is still outstanding, so keep hunting.
func TestClassifyStreakBucket_PartiallyWatched_IsCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	ch.StreamWatched = streakWatchTarget - time.Minute
	if !isStreakCandidate(ch.Snapshot(), "") {
		t.Error("stream watched below the target should remain a candidate")
	}
}

func TestClassifyStreakBucket_Offline_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	// never set online
	if isStreakCandidate(ch.Snapshot(), "") {
		t.Error("offline channel should not be streak candidate")
	}
}

func TestClassifyStreakBucket_DropOwnedByDropsWatcher_NotCandidate(t *testing.T) {
	ch := channels.NewState("alice", "Alice", "111")
	ch.SetPriority(2)
	ch.SetOnline("b1", "G", 5)
	// drops watcher owns this channel — must be skipped
	if isStreakCandidate(ch.Snapshot(), "111") {
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
	if !isStreakCandidate(ch.Snapshot(), "") {
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

// TestSortStreakCandidates_ClosestToTargetFirst: a scarce slot should
// finish the streak that's nearly earned rather than start a fresh one,
// so accumulated watch time outranks OnlineSince.
func TestSortStreakCandidates_ClosestToTargetFirst(t *testing.T) {
	// chEarly went live first but has no watch time; chNearly went live
	// later yet is one minute from the target.
	chEarly := channels.NewState("early", "Early", "1")
	chEarly.SetPriority(2)
	chEarly.SetOnline("b1", "G", 5)
	time.Sleep(2 * time.Millisecond)
	chNearly := channels.NewState("nearly", "Nearly", "2")
	chNearly.SetPriority(2)
	chNearly.SetOnline("b2", "G", 5)
	chNearly.StreamWatched = streakWatchTarget - time.Minute

	in := []*channels.State{chEarly, chNearly}
	sortStreakCandidates(in)

	if in[0].ChannelID != "2" {
		t.Errorf("expected the nearly-finished candidate (id=2) first, got id=%s",
			in[0].ChannelID)
	}
}

func TestSelectFillCandidates_StreakBeforeViewerCount(t *testing.T) {
	// Two online unwatched channels:
	//   bigViewer: 1000 viewers, no streak candidacy (P2)
	//   freshLive: 10 viewers, streak candidate (just went online)
	// FillSpadeSlots must pick freshLive first, not bigViewer.
	bigViewer := channels.NewState("big", "Big", "1")
	bigViewer.SetPriority(2)
	bigViewer.SetOnline("b1", "G", 1000)
	bigViewer.MarkStreakClaimed() // already claimed → not a streak candidate

	freshLive := channels.NewState("fresh", "Fresh", "2")
	freshLive.SetPriority(2)
	freshLive.SetOnline("b2", "G", 10)
	// StreakClaimedAt left zero → unclaimed → streak candidate

	candidates := []*channels.State{bigViewer, freshLive}
	ordered := orderFillCandidates(candidates, "")

	if len(ordered) != 2 {
		t.Fatalf("got %d candidates, want 2", len(ordered))
	}
	if ordered[0].ChannelID != "2" {
		t.Errorf("expected fresh streak candidate (id=2) first, got id=%s",
			ordered[0].ChannelID)
	}
	if ordered[1].ChannelID != "1" {
		t.Errorf("expected viewer-count fallback (id=1) second, got id=%s",
			ordered[1].ChannelID)
	}
}

func TestSelectFillCandidates_NoStreakCandidates_FallsBackToViewerCount(t *testing.T) {
	chSmall := channels.NewState("small", "Small", "1")
	chSmall.SetPriority(2)
	chSmall.SetOnline("b1", "G", 10)
	chSmall.MarkStreakClaimed()

	chBig := channels.NewState("big", "Big", "2")
	chBig.SetPriority(2)
	chBig.SetOnline("b2", "G", 1000)
	chBig.MarkStreakClaimed()

	ordered := orderFillCandidates([]*channels.State{chSmall, chBig}, "")

	if ordered[0].ChannelID != "2" {
		t.Errorf("no streak candidates: expected viewer-count desc, got id=%s first",
			ordered[0].ChannelID)
	}
}
