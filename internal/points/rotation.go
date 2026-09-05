package points

import (
	"sort"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
)

// rotationInterval is how often RotationLoop re-evaluates the 2-Spade-
// slot allocation. Twitch only credits channel-points-WATCH for ~2
// channels at a time, so we cycle through the rotation pool to give
// each channel airtime over a session.
const rotationInterval = 5 * time.Minute

// maxSpadeSlots is the channel-points-WATCH capacity (priority allocation
// fills these slots: P0 active drop → P1 always-watch → P2 rotate). The
// drops Watcher owns its picked channel exclusively and is NOT counted
// against this limit — it runs on the GQL sendSpadeEvents pipeline,
// not the legacy POST-spade pipeline that the Spade tracker uses.
const maxSpadeSlots = 2

// streakWatchTarget is how much accumulated watch time a stream needs
// before we stop hunting its WATCH_STREAK. Twitch counts a broadcast
// towards a viewer's streak once it has been watched for ~10 minutes;
// we hunt a little past that so a channel whose bonus is slow to land
// isn't dropped one minute early.
//
// This gates on WATCH TIME, deliberately not on wall-clock time since
// stream start. Twitch's other "30 minutes" rule — the gap required
// between two broadcasts for the next one to count as a separate
// stream — is about stream boundaries, NOT a deadline for earning the
// streak. An earlier version of this file conflated the two and gave
// up on any channel more than 30 minutes into its stream: with only
// maxSpadeSlots channels creditable at once, every channel that went
// live while the slots were busy was abandoned for that whole stream,
// its streak broke, and the next stream started from zero. The bonus
// stays earnable for as long as the stream runs, so the only thing
// that should retire a candidate is having actually watched it.
const streakWatchTarget = 12 * time.Minute

// minPointsHold is the shortest slot worth giving a channel at all.
// Twitch credits channel-points WATCH only after ~5 minutes of continuous
// viewing, so a channel rotated out before that earns nothing — the slot
// time is simply thrown away. Observed on 2026-09-05: with ~6 channels
// live at once and 480 slot-minutes available (30% utilisation), five
// channels still ended a multi-hour stream with ZERO watch credits
// because the rotation kept handing everyone slices of a minute or two.
const minPointsHold = 6 * time.Minute

// holdSlot reports whether a channel that already holds a Spade slot must
// keep it this cycle instead of being rotated out. This turns the
// rotation from "give everyone a sliver" into "finish one, then the next":
//
//   - A streak candidate keeps its slot until it has watched enough for
//     the bonus (isStreakCandidate goes false at streakWatchTarget), so a
//     streak is actually completed rather than approached repeatedly.
//   - Every other channel keeps its slot until one points interval has had
//     a chance to land (minPointsHold).
//
// Neither hold can last: both gates read watch time that only grows while
// the channel is on air, so a held slot is always released after at most
// streakWatchTarget. The drop pick (P0) still outranks a hold.
func holdSlot(snap channels.Snapshot, dropChanID string) bool {
	if !snap.IsWatching {
		return false
	}
	if isStreakCandidate(snap, dropChanID) {
		return true
	}
	if !snap.WatchingSince.IsZero() && time.Since(snap.WatchingSince) < minPointsHold {
		return true
	}
	return false
}

// isStreakCandidate reports whether a channel is eligible for the
// Streak-Hunt bucket: online, not owned by the drops watcher, hasn't
// claimed THIS stream's WATCH_STREAK yet, and hasn't already been
// watched long enough for the bonus to have landed.
func isStreakCandidate(snap channels.Snapshot, dropChanID string) bool {
	if !snap.IsOnline {
		return false
	}
	if snap.ChannelID == dropChanID {
		return false
	}
	if !snap.StreakClaimedAt.Before(snap.OnlineSince) {
		// Already claimed (or claimed exactly when online — treat as claimed).
		return false
	}
	// Retire the candidate only once we have genuinely watched the
	// stream long enough that Twitch should have granted the bonus.
	// Channels that never got a slot keep their claim on one no matter
	// how long the stream has been running (see streakWatchTarget).
	if snap.StreamWatched >= streakWatchTarget {
		return false
	}
	return true
}

// sortStreakCandidates orders by accumulated watch time DESC — the
// candidate closest to the streak target goes first, so a scarce slot
// finishes a streak instead of leaving several channels stranded
// half-way. Ties (notably the common all-zero case, where every
// candidate is freshly online) fall back to OnlineSince ASC so the
// longest-waiting stream is served first, then ChannelID for
// deterministic ordering.
func sortStreakCandidates(list []*channels.State) {
	sort.Slice(list, func(i, j int) bool {
		si := list[i].Snapshot()
		sj := list[j].Snapshot()
		if si.StreamWatched != sj.StreamWatched {
			return si.StreamWatched > sj.StreamWatched
		}
		if !si.OnlineSince.Equal(sj.OnlineSince) {
			return si.OnlineSince.Before(sj.OnlineSince)
		}
		return list[i].ChannelID < list[j].ChannelID
	})
}

// RotationLoop runs Rotate every rotationInterval until stopCh fires.
// Started as a goroutine from Farmer.Start.
func (s *Service) RotationLoop(stopCh <-chan struct{}) {
	ticker := time.NewTicker(rotationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.Rotate()
		case <-stopCh:
			return
		}
	}
}

// Rotate computes the desired 2-channel watch set and diffs it against
// what Spade is currently watching: stops anything that fell out, keeps
// anything that stays (refreshing the broadcast ID), starts anything
// new. drops.ServiceDeps.TriggerRotation points here so the points-
// rotation immediately reflects a fresh drop pick rather than waiting
// up to 5 min for the next ticker.
//
// The drops Watcher's currently-picked channel is explicitly skipped —
// drops owns it via the GQL sendSpadeEvents pipeline; double-tracking
// it via the Spade POST endpoint would create cross-talk and may flag
// the user as suspicious.
func (s *Service) Rotate() {
	dropChanID := ""
	if s.dropWatch != nil {
		dropChanID = s.dropWatch.CurrentChannelID()
	}

	var priority0 []*channels.State      // P0: active drop (auto-promoted)
	var priorityStreak []*channels.State // PS: fresh-online, unclaimed streak (NEW)
	var priority1 []*channels.State
	var priority2 []*channels.State
	for _, ch := range s.channels.States() {
		snap := ch.Snapshot()
		if !snap.IsOnline {
			continue
		}
		if snap.ChannelID == dropChanID {
			continue // drops Watcher owns this — don't add to Spade rotation
		}
		// Drops auto-promote to P0; keeps existing precedence rule
		// (a channel with both an active drop AND an unclaimed streak
		// goes to P0 — drops are typically worth more than 450 points).
		if snap.HasActiveDrop {
			priority0 = append(priority0, ch)
			continue
		}
		// Streak-Hunt sits between P0 and P1 — fresh-online, unclaimed.
		if isStreakCandidate(snap, dropChanID) {
			priorityStreak = append(priorityStreak, ch)
			continue
		}
		if snap.Priority == 1 {
			priority1 = append(priority1, ch)
		} else {
			priority2 = append(priority2, ch)
		}
	}

	// Sort P0 by campaign end time (soonest expiring first gets the Spade slot).
	sort.Slice(priority0, func(i, j int) bool {
		ei := s.drops.CampaignEndAt(priority0[i].Snapshot().CampaignID)
		ej := s.drops.CampaignEndAt(priority0[j].Snapshot().CampaignID)
		if ei.IsZero() {
			return false
		}
		if ej.IsZero() {
			return true
		}
		if ei.Equal(ej) {
			return priority0[i].ChannelID < priority0[j].ChannelID
		}
		return ei.Before(ej)
	})
	sort.Slice(priority1, func(i, j int) bool {
		return priority1[i].ChannelID < priority1[j].ChannelID
	})
	sort.Slice(priority2, func(i, j int) bool {
		return priority2[i].ChannelID < priority2[j].ChannelID
	})

	// Build the desired watch set: P0 → PS → P1 → P2 (rotated cursor).
	desired := make(map[string]*channels.State)

	// Since 2026-07-10 the drop pick needs a Spade heartbeat slot of its
	// own (drop credit moved onto the Spade POST pipeline — see
	// drops/apply.go step 8), so rotation must leave one slot free while
	// a pick is active. The pick itself is excluded from the lists above.
	slotLimit := maxSpadeSlots
	if dropChanID != "" {
		slotLimit--
	}

	slotsUsed := 0
	for _, ch := range priority0 {
		if slotsUsed >= slotLimit {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	// Minimum-hold pass: channels already on air that have not yet been
	// watched long enough to earn anything keep their slot before any new
	// channel is considered (see holdSlot). Without this the round-robin
	// reshuffles every cycle and nobody reaches Twitch's credit threshold.
	var pinned []*channels.State
	for _, list := range [][]*channels.State{priorityStreak, priority1, priority2} {
		for _, ch := range list {
			if _, already := desired[ch.ChannelID]; already {
				continue
			}
			if holdSlot(ch.Snapshot(), dropChanID) {
				pinned = append(pinned, ch)
			}
		}
	}
	// Closest to its target first, so a scarce slot finishes someone.
	sortStreakCandidates(pinned)
	for _, ch := range pinned {
		if slotsUsed >= slotLimit {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	// Streak-Hunt: closest-to-target first (see sortStreakCandidates).
	// Doesn't starve P1/P2 indefinitely: a candidate holding a slot
	// accumulates watch time every tick, so it either claims its bonus or
	// reaches streakWatchTarget and retires to P2. The queue therefore
	// drains at roughly streakWatchTarget per slot per candidate, instead
	// of every candidate being abandoned once the stream got old.
	sortStreakCandidates(priorityStreak)
	for _, ch := range priorityStreak {
		if slotsUsed >= slotLimit {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	for _, ch := range priority1 {
		if slotsUsed >= slotLimit {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	remainingSlots := slotLimit - slotsUsed
	if remainingSlots > 0 && len(priority2) > 0 {
		s.mu.Lock()
		idx := s.rotationIndex % len(priority2)
		s.rotationIndex = (s.rotationIndex + remainingSlots) % len(priority2)
		s.mu.Unlock()

		for i := 0; i < remainingSlots && i < len(priority2); i++ {
			ch := priority2[(idx+i)%len(priority2)]
			desired[ch.ChannelID] = ch
		}
	}

	// Diff vs what's currently watching: stop anything that fell out,
	// keep anything that stays (and refresh its broadcast ID in case the
	// streamer restarted mid-cycle).
	currentlyWatching := make(map[string]bool)
	for _, list := range [][]*channels.State{priority0, priorityStreak, priority1, priority2} {
		for _, ch := range list {
			if !ch.Snapshot().IsWatching {
				continue
			}
			currentlyWatching[ch.ChannelID] = true
			if _, keep := desired[ch.ChannelID]; !keep {
				s.spade.StopWatching(ch.ChannelID)
				s.prober.Stop(ch.Login)
				ch.SetWatching(false)
			} else {
				snap := ch.Snapshot()
				s.spade.UpdateBroadcastID(snap.ChannelID, snap.BroadcastID, snap.GameName, snap.GameID)
			}
		}
	}

	// Start newly desired channels.
	for chID, ch := range desired {
		if currentlyWatching[chID] {
			continue
		}
		snap := ch.Snapshot()
		broadcastID := snap.BroadcastID
		if broadcastID == "" {
			go s.fetchAndStartWatching(ch)
			continue
		}
		if s.spade.StartWatching(snap.ChannelID, snap.Login, broadcastID, snap.GameName, snap.GameID) {
			ch.SetWatching(true)
			s.prober.Start(snap.Login)
			s.log("Started watching %s (broadcast=%s, via rotation)", snap.DisplayName, broadcastID)
		} else {
			s.log("[Spade] StartWatching for %s returned false (capacity full)", snap.DisplayName)
		}
	}
}

// fetchAndStartWatching fills in a missing broadcast ID via GQL before
// starting Spade. Called as a goroutine from Rotate when a desired
// channel's State has an empty BroadcastID — usually right after a
// streamer toggles online, before the channel-info refresh has caught
// up.
func (s *Service) fetchAndStartWatching(ch *channels.State) {
	info, err := s.gql.GetChannelInfo(ch.Login)
	if err != nil {
		s.log("[Spade] failed to fetch broadcast ID for %s: %v", ch.DisplayName, err)
		return
	}
	if info.BroadcastID == "" {
		s.log("[Spade] %s has empty broadcast ID, skipping", ch.DisplayName)
		return
	}
	ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount, info.StreamCreatedAt)
	if s.spade.StartWatching(ch.ChannelID, ch.Login, info.BroadcastID, info.GameName, info.GameID) {
		ch.SetWatching(true)
		s.prober.Start(ch.Login)
		s.log("Started watching %s (broadcast=%s)", ch.DisplayName, info.BroadcastID)
	}
}

// TryStartWatching is the points-side single-channel start path: used by
// Farmer when a channel is added (addChannelWithInfo) or comes online
// (EventStreamUp). It refuses to double-track the drops Watcher's
// current pick — drops has exclusive ownership of that channel.
func (s *Service) TryStartWatching(state *channels.State) {
	snap := state.Snapshot()
	if !snap.IsOnline || snap.IsWatching {
		return
	}

	if s.dropWatch != nil && s.dropWatch.CurrentChannelID() == snap.ChannelID {
		return
	}

	if snap.BroadcastID == "" {
		s.log("[Spade] skipping %s — no broadcast ID", snap.DisplayName)
		return
	}

	if s.spade.StartWatching(snap.ChannelID, snap.Login, snap.BroadcastID, snap.GameName, snap.GameID) {
		state.SetWatching(true)
		s.prober.Start(snap.Login)
		s.log("Started watching %s (Spade active, broadcast=%s)", snap.DisplayName, snap.BroadcastID)
	}
}

// orderFillCandidates returns the input list sorted by:
//  1. Streak-Hunt candidates first (closest to streakWatchTarget first)
//  2. Everything else by ViewerCount DESC (existing behavior)
//
// Pure function for testability — caller passes dropChanID.
func orderFillCandidates(in []*channels.State, dropChanID string) []*channels.State {
	var streak, rest []*channels.State
	for _, ch := range in {
		if isStreakCandidate(ch.Snapshot(), dropChanID) {
			streak = append(streak, ch)
		} else {
			rest = append(rest, ch)
		}
	}
	sortStreakCandidates(streak)
	sort.Slice(rest, func(i, j int) bool {
		return rest[i].Snapshot().ViewerCount > rest[j].Snapshot().ViewerCount
	})
	return append(streak, rest...)
}

// FillSpadeSlots scans for online-but-not-watching channels and tops up
// the Spade tracker until it's at capacity. Called by farmer after
// EventStreamDown frees a slot AND by the WATCH_STREAK event handler
// to immediately rotate in the next streak candidate.
//
// Selection order: Streak-Hunt candidates first (closest to the watch
// target), then remaining channels by ViewerCount desc.
func (s *Service) FillSpadeSlots() {
	dropChanID := ""
	if s.dropWatch != nil {
		dropChanID = s.dropWatch.CurrentChannelID()
	}

	var candidates []*channels.State
	for _, ch := range s.channels.States() {
		snap := ch.Snapshot()
		if snap.IsOnline && !snap.IsWatching {
			candidates = append(candidates, ch)
		}
	}

	for _, ch := range orderFillCandidates(candidates, dropChanID) {
		if s.spade.ActiveSlots() <= 0 {
			break
		}
		s.TryStartWatching(ch)
	}
}
