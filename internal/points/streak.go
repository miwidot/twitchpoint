package points

import (
	"fmt"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
)

// Watch-streak status for display.
//
// Twitch does NOT tell us the streak COUNT. The number the website shows
// ("N streams in a row") is not in any field we can reach: on 2026-11-13
// every plausible name was probed against the type that carries the
// viewer's points data (CommunityPointsProperties) and all were rejected,
// and schema introspection is disabled. What Twitch does give us is the
// bonus EVENT, over PubSub with reason code WATCH_STREAK.
//
// So this reports something different, and more useful while the miner is
// running: where each channel stands in the hunt for the CURRENT
// broadcast's bonus. Every value comes from state we already keep, so
// nothing here costs a request.
//
// The phases deliberately mirror the rotation's own decisions
// (isStreakCandidate, which is what actually allocates the slots) so the
// display cannot claim one thing while the allocator does another.

// StreakPhase is where a channel stands for the current broadcast.
type StreakPhase int

const (
	// StreakIdle: nothing to show. The channel is offline, or it belongs
	// to the drops watcher, which never enters the streak bucket.
	StreakIdle StreakPhase = iota

	// StreakHunting: a candidate that currently holds a watch slot and is
	// accumulating the time the bonus needs.
	StreakHunting

	// StreakQueued: a candidate with no slot. It keeps its claim on the
	// next free one no matter how long the broadcast has been running.
	StreakQueued

	// StreakClaimed: the WATCH_STREAK bonus arrived for this broadcast.
	StreakClaimed

	// StreakNoBonus: watched past the target and no bonus came. Usually
	// normal — Twitch only pays the bonus when a streak is actually being
	// continued — but it also covers the case where the slot came too
	// late to count.
	StreakNoBonus
)

// String is the stable key used for styling in the web UI. Idle is the
// empty string so a caller can test it with a plain truthiness check.
func (p StreakPhase) String() string {
	switch p {
	case StreakHunting:
		return "hunting"
	case StreakQueued:
		return "queued"
	case StreakClaimed:
		return "claimed"
	case StreakNoBonus:
		return "no_bonus"
	default:
		return ""
	}
}

// StreakStatus is one channel's streak situation, ready to display.
type StreakStatus struct {
	Phase StreakPhase

	// Watched is the time accumulated on the current broadcast, and
	// Target how much of it the bonus needs (streakWatchTarget). Both are
	// carried so a caller can draw a progress bar without importing the
	// constant, and so the two can never disagree with the rotation.
	Watched time.Duration
	Target  time.Duration
}

// Label is the text to print. Kept here rather than in each front-end so
// the TUI and the web dashboard cannot drift into different wording.
func (s StreakStatus) Label() string {
	switch s.Phase {
	case StreakHunting:
		return fmt.Sprintf("%s/%s", mmss(s.Watched), mmss(s.Target))
	case StreakQueued:
		return "queued"
	case StreakClaimed:
		return "claimed"
	case StreakNoBonus:
		return "no bonus"
	default:
		return ""
	}
}

// mmss formats a duration as m:ss, which is how the target (12 min) and
// the progress towards it read most naturally. Negative input is clamped
// to zero rather than printing a minus sign into a progress cell.
func mmss(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// StreakStatus describes one channel for the UI. It resolves the
// drops-owned channel itself, so callers do not have to know that the
// drops watcher holds a channel outside the points rotation.
func (s *Service) StreakStatus(snap channels.Snapshot) StreakStatus {
	dropChanID := ""
	if s.dropWatch != nil {
		dropChanID = s.dropWatch.CurrentChannelID()
	}
	return describeStreak(snap, dropChanID)
}

// describeStreak is the pure form, so the phase rules can be tested
// without a Service.
//
// The order of the checks matters: "claimed" is tested before anything
// else because once the bonus has landed the remaining questions (slot,
// watch time) no longer say anything useful about this broadcast.
func describeStreak(snap channels.Snapshot, dropChanID string) StreakStatus {
	st := StreakStatus{Watched: snap.StreamWatched, Target: streakWatchTarget}

	if !snap.IsOnline {
		return st
	}

	// Same comparison the rotation uses: a claim older than the current
	// broadcast belongs to the previous one.
	if !snap.StreakClaimedAt.Before(snap.OnlineSince) {
		st.Phase = StreakClaimed
		return st
	}

	// The drops watcher owns this channel; the rotation never puts it in
	// the streak bucket, so calling it queued would be a lie.
	if snap.ChannelID == dropChanID {
		return st
	}

	if snap.StreamWatched >= streakWatchTarget {
		st.Phase = StreakNoBonus
		return st
	}

	if snap.IsWatching {
		st.Phase = StreakHunting
		return st
	}
	st.Phase = StreakQueued
	return st
}
