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

// streakHuntWindow is the cutoff past which a channel is no longer
// considered for the Streak-Hunt slot. Twitch's WATCH_STREAK bonus
// fires after ~5min watch-time once a streak >=5 is established;
// past 30min from stream-start the window is almost always closed,
// so further blocking the slot for this channel wastes bandwidth.
const streakHuntWindow = 30 * time.Minute

// isStreakCandidate reports whether a channel is eligible for the
// Streak-Hunt bucket: online, not owned by the drops watcher, hasn't
// claimed THIS stream's WATCH_STREAK yet, and is within the hunt
// window. Pure function — caller passes "now" for testability.
func isStreakCandidate(snap channels.Snapshot, now time.Time, dropChanID string) bool {
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
	if now.Sub(snap.OnlineSince) >= streakHuntWindow {
		return false
	}
	return true
}

// sortStreakCandidates orders by OnlineSince ASC — oldest stream first,
// since its 30min window is closest to expiring. Stable tie-break by
// ChannelID for deterministic ordering when two candidates share a
// timestamp (rare but possible in test setups).
func sortStreakCandidates(list []*channels.State) {
	sort.Slice(list, func(i, j int) bool {
		si := list[i].Snapshot()
		sj := list[j].Snapshot()
		if si.OnlineSince.Equal(sj.OnlineSince) {
			return list[i].ChannelID < list[j].ChannelID
		}
		return si.OnlineSince.Before(sj.OnlineSince)
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

	now := time.Now()

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
		if isStreakCandidate(snap, now, dropChanID) {
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

	slotsUsed := 0
	for _, ch := range priority0 {
		if slotsUsed >= maxSpadeSlots {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	// Streak-Hunt: FIFO by OnlineSince ASC. Never starves P1/P2 long-term
	// because each candidate either claims (within ~5-15min) or times out
	// (30min hard cap), then drops back to P2 next tick.
	sortStreakCandidates(priorityStreak)
	for _, ch := range priorityStreak {
		if slotsUsed >= maxSpadeSlots {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	for _, ch := range priority1 {
		if slotsUsed >= maxSpadeSlots {
			break
		}
		desired[ch.ChannelID] = ch
		slotsUsed++
	}

	remainingSlots := maxSpadeSlots - slotsUsed
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
				s.spade.UpdateBroadcastID(snap.ChannelID, snap.BroadcastID)
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
	ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
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

// FillSpadeSlots scans for online-but-not-watching channels and tops up
// the Spade tracker until it's at capacity. Called by farmer after
// EventStreamDown frees a slot — without it, a streamer going offline
// just leaves a slot empty until the next 5-min Rotate tick.
//
// Sorts candidates by viewer count (popular channels first) so a
// flap-and-recover doesn't push a heavy-traffic channel out of rotation
// in favor of a low-traffic one that happened to come online first.
func (s *Service) FillSpadeSlots() {
	var candidates []*channels.State
	for _, ch := range s.channels.States() {
		snap := ch.Snapshot()
		if snap.IsOnline && !snap.IsWatching {
			candidates = append(candidates, ch)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Snapshot().ViewerCount > candidates[j].Snapshot().ViewerCount
	})

	for _, ch := range candidates {
		if s.spade.ActiveSlots() <= 0 {
			break
		}
		s.TryStartWatching(ch)
	}
}
