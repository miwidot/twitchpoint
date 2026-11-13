package points

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
)

func TestDescribeStreak_Phases(t *testing.T) {
	now := time.Now()
	online := now.Add(-40 * time.Minute)

	cases := []struct {
		name       string
		snap       channels.Snapshot
		dropChanID string
		want       StreakPhase
		wantLabel  string
	}{
		{
			name: "offline channel has nothing to hunt",
			snap: channels.Snapshot{ChannelID: "1", IsOnline: false},
			want: StreakIdle,
		},
		{
			name: "online, no slot yet",
			snap: channels.Snapshot{ChannelID: "1", IsOnline: true, OnlineSince: online},
			want: StreakQueued, wantLabel: "queued",
		},
		{
			name: "holding a slot, part way to the target",
			snap: channels.Snapshot{
				ChannelID: "1", IsOnline: true, IsWatching: true,
				OnlineSince: online, StreamWatched: 8*time.Minute + 12*time.Second,
			},
			want: StreakHunting, wantLabel: "8:12/12:00",
		},
		{
			name: "bonus arrived for this broadcast",
			snap: channels.Snapshot{
				ChannelID: "1", IsOnline: true, IsWatching: true,
				OnlineSince: online, StreakClaimedAt: online.Add(6 * time.Minute),
			},
			want: StreakClaimed, wantLabel: "claimed",
		},
		{
			// The claim belongs to the PREVIOUS broadcast, so this one is
			// still open. Getting this wrong would make the miner skip the
			// hunt for every channel that ever earned a streak.
			name: "claim predates the current broadcast",
			snap: channels.Snapshot{
				ChannelID: "1", IsOnline: true,
				OnlineSince: online, StreakClaimedAt: online.Add(-3 * time.Hour),
			},
			want: StreakQueued, wantLabel: "queued",
		},
		{
			name: "watched past the target without a bonus",
			snap: channels.Snapshot{
				ChannelID: "1", IsOnline: true, IsWatching: true,
				OnlineSince: online, StreamWatched: 20 * time.Minute,
			},
			want: StreakNoBonus, wantLabel: "no bonus",
		},
		{
			// The drops watcher owns it outside the points rotation, so it
			// is never a streak candidate — reporting "queued" would
			// promise a slot that is never coming.
			name: "channel owned by the drops watcher",
			snap: channels.Snapshot{
				ChannelID: "42", IsOnline: true, IsWatching: true, OnlineSince: online,
			},
			dropChanID: "42",
			want:       StreakIdle,
		},
		{
			// Claimed outranks drop ownership: the bonus really did land,
			// and hiding it would be wrong.
			name: "drops channel that already got the bonus",
			snap: channels.Snapshot{
				ChannelID: "42", IsOnline: true, IsWatching: true,
				OnlineSince: online, StreakClaimedAt: online.Add(time.Minute),
			},
			dropChanID: "42",
			want:       StreakClaimed, wantLabel: "claimed",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := describeStreak(c.snap, c.dropChanID)
			if got.Phase != c.want {
				t.Errorf("phase = %v (%q), want %v", got.Phase, got.Phase, c.want)
			}
			if got.Label() != c.wantLabel {
				t.Errorf("label = %q, want %q", got.Label(), c.wantLabel)
			}
			if got.Target != streakWatchTarget {
				t.Errorf("target = %v, want the rotation's %v", got.Target, streakWatchTarget)
			}
		})
	}
}

// The display must agree with the allocator: anything the rotation treats
// as a streak candidate has to show as hunting or queued, and anything it
// does not must show as neither. If these two ever disagree, the UI is
// lying about what the miner is doing.
func TestDescribeStreak_AgreesWithRotation(t *testing.T) {
	now := time.Now()
	online := now.Add(-30 * time.Minute)

	snaps := []channels.Snapshot{
		{ChannelID: "1", IsOnline: false},
		{ChannelID: "1", IsOnline: true, OnlineSince: online},
		{ChannelID: "1", IsOnline: true, IsWatching: true, OnlineSince: online, StreamWatched: time.Minute},
		{ChannelID: "1", IsOnline: true, IsWatching: true, OnlineSince: online, StreamWatched: streakWatchTarget},
		{ChannelID: "1", IsOnline: true, OnlineSince: online, StreakClaimedAt: online.Add(time.Minute)},
		{ChannelID: "42", IsOnline: true, OnlineSince: online},
	}

	for _, snap := range snaps {
		candidate := isStreakCandidate(snap, "42")
		phase := describeStreak(snap, "42").Phase
		shown := phase == StreakHunting || phase == StreakQueued
		if candidate != shown {
			t.Errorf("isStreakCandidate=%v but phase=%q for %+v", candidate, phase, snap)
		}
	}
}

func TestMMSS(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                  "0:00",
		9 * time.Second:                    "0:09",
		12 * time.Minute:                   "12:00",
		8*time.Minute + 12*time.Second:     "8:12",
		90 * time.Minute:                   "90:00",
		-5 * time.Second:                   "0:00", // clamped, never "-0:05"
		time.Minute + 999*time.Millisecond: "1:00", // truncates, never rolls to 1:60
	}
	for d, want := range cases {
		if got := mmss(d); got != want {
			t.Errorf("mmss(%v) = %q, want %q", d, got, want)
		}
	}
}
