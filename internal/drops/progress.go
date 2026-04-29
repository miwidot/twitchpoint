package drops

import (
	"fmt"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// SilentPickThreshold is how long a pick can go without
// ApplyProgressUpdate firing (no successful CurrentDrop session, no WS
// progress event) before PollProgressOnce flags it as un-credited and
// kicks it onto the manual cooldown. 3 minutes is past the
// "first 1-2 cycles after fresh pick" Twitch warmup window but well
// under the 15-min full inventory cycle, so the bot drops a stuck pick
// long before the next CheckLoop tick. TDM uses a similar order of
// magnitude (its DROP_VERIFICATION_INTERVAL is in the same range).
const SilentPickThreshold = 3 * time.Minute

// ProgressPollLoop polls DropCurrentSessionContext every 60 seconds for
// the currently picked drop channel. This is the bridge that keeps
// progress in sync when user-drop-events PubSub is silent (which is
// most of the time per TwitchDropsMiner research). Pass the farmer's
// stop channel so the loop exits at shutdown.
func (s *Service) ProgressPollLoop(stopCh <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.PollProgressOnce()
		case <-stopCh:
			return
		}
	}
}

// PollProgressOnce queries DropCurrentSession for the current pick and
// applies the result. Idempotent — safe to call frequently.
//
// Per TDM watch_service.py:205 (minute_almost_done): skip the GQL call
// if a WebSocket update arrived in the last 30s — no point re-fetching
// what we just got pushed. Cuts the DropCurrentSession query rate
// roughly in half when WebSocket is healthy.
func (s *Service) PollProgressOnce() {
	s.mu.RLock()
	pickedID := s.currentPickID
	recentUpdate := !s.lastProgressUpdate.IsZero() && time.Since(s.lastProgressUpdate) < 30*time.Second
	s.mu.RUnlock()
	pickedCampID := s.Stall.LastPickCampaignID()

	if pickedID == "" || pickedCampID == "" {
		return
	}
	if recentUpdate {
		return
	}

	session, err := s.gql.GetCurrentDropSession(pickedID)
	if err != nil {
		// Silent on the UI feed — this happens during stream
		// offline/transition; file log is enough.
		if s.writeLogFile != nil {
			s.writeLogFile(fmt.Sprintf("[Drops/Poll] CurrentDrop fetch failed for pick=%s campaign=%s: %v",
				pickedID, pickedCampID, err))
		}
		return
	}
	if session == nil {
		// Twitch reports no active drop session for this channel.
		// Two common causes: (1) drop just hit 100% and Twitch is
		// silent until inventory advances; (2) the pick is genuinely
		// not crediting (stale broadcast, anti-cheat soft-throttle,
		// daily-rolling campaign without a fresh slot, …).
		//
		// LastProgressUpdate is reset at end of ApplyPick and again
		// on every ApplyProgressUpdate, so silentFor measures
		// "wall-clock time since the last good signal for THIS pick".
		// Past SilentPickThreshold, push the pick onto the manual
		// cooldown and re-run ProcessDrops out-of-band — the next
		// queued campaign gets the slot without waiting for the
		// 15-min CheckLoop.
		s.mu.RLock()
		silentFor := time.Since(s.lastProgressUpdate)
		s.mu.RUnlock()

		if silentFor > SilentPickThreshold {
			s.log("[Drops/Poll] pick=%s silent %v — manual cooldown 15min, re-picking",
				pickedID, silentFor.Round(time.Second))
			s.Stall.SetManual(pickedID, 15*time.Minute)
			go s.ProcessDrops()
			return
		}

		if s.writeLogFile != nil {
			s.writeLogFile(fmt.Sprintf("[Drops/Poll] no session for pick=%s campaign=%s — Twitch returned nil (%v silent, threshold %v)",
				pickedID, pickedCampID, silentFor.Round(time.Second), SilentPickThreshold))
		}
		return
	}

	s.ApplyProgressUpdate(twitch.DropProgressData{
		CampaignID:             pickedCampID,
		DropID:                 session.DropID,
		CurrentMinutesWatched:  session.CurrentMinutesWatched,
		RequiredMinutesWatched: session.RequiredMinutesWatched,
	})

	// When poll says the current drop is at 100%, do TWO things:
	// 1. Try MarkCompletedIfFinishedExternally — fetches inventory + only
	//    marks completed if the campaign is genuinely no longer in
	//    progress (i.e., user really finished all drops). For multi-drop
	//    campaigns where one drop is done but more are pending, the
	//    campaign WILL still be in inventory progress, so it stays
	//    un-completed.
	// 2. Trigger processDrops so the selector re-evaluates (next drop
	//    in queue gets picked if this one is done, etc).
	if session.RequiredMinutesWatched > 0 && session.CurrentMinutesWatched >= session.RequiredMinutesWatched {
		if s.writeLogFile != nil {
			s.writeLogFile(fmt.Sprintf("[Drops/Poll] drop complete on campaign %s (%d/%d)",
				pickedCampID, session.CurrentMinutesWatched, session.RequiredMinutesWatched))
		}
		go func() {
			s.MarkCompletedIfFinishedExternally(pickedCampID)
			s.ProcessDrops()
		}()
	}
}

// HandleDropClaim is the sequential, TDM-aligned drop-claim flow. It:
//  1. Claims the drop (synchronous — must succeed before we re-evaluate state)
//  2. Sleeps 4s (Twitch's backend takes a moment to advance the drop session)
//  3. Polls DropCurrentSession up to 8× (with 2s sleep) waiting for the
//     dropID to change — i.e. Twitch has advanced to the next drop in
//     the campaign or the campaign is now done
//  4. Triggers processDrops to re-pick / mark completed
//
// This sequencing prevents the v1.8.0 race where parallel claim +
// processDrops goroutines saw stale unclaimed state.
func (s *Service) HandleDropClaim(data twitch.DropClaimData) {
	if data.DropInstanceID != "" {
		if err := s.gql.ClaimDrop(data.DropInstanceID); err != nil {
			s.log("[Drops/WS] Failed to claim drop: %v", err)
		} else {
			s.log("[Drops/WS] Claimed drop instance %s", data.DropInstanceID)
		}
	}

	// Wait for Twitch to advance the session.
	time.Sleep(4 * time.Second)

	s.mu.RLock()
	pickedID := s.currentPickID
	s.mu.RUnlock()

	if pickedID != "" && data.DropID != "" {
		for i := 0; i < 8; i++ {
			session, err := s.gql.GetCurrentDropSession(pickedID)
			if err != nil {
				break
			}
			if session == nil || session.DropID != data.DropID {
				// Either no more session for this channel (campaign done)
				// or session has advanced to the next drop.
				break
			}
			time.Sleep(2 * time.Second)
		}
	}

	s.ProcessDrops()
}

// ApplyProgressUpdate handles a WebSocket drop-progress event by
// updating the in-memory ActiveDrops slice (so the web/TUI sees the
// new value within 1 second instead of waiting for the next 15-min
// poll). Also updates the matching channel's HasActiveDrop progress
// for TUI rendering.
//
// Per spec section 6.6: matches by (CampaignID, DropID). When the
// payload's DropID differs from the cached ActiveDrop's drop, the
// cached row's DropName + Required + Progress are all replaced from
// the payload (via a lookup against CampaignCache to find the
// human-readable drop name).
//
// Per TDM message_handlers.py:251 (drop.can_earn before update_minutes):
// skip the update entirely if the campaign is currently disabled or
// completed — the payload is for a drop we shouldn't be earning,
// displaying it would mislead the user.
func (s *Service) ApplyProgressUpdate(data twitch.DropProgressData) {
	// can_earn equivalent: skip if campaign isn't currently farmable.
	if s.cfg.IsCampaignDisabled(data.CampaignID) || s.cfg.IsCampaignCompleted(data.CampaignID) {
		return
	}
	// Resolve the payload's drop name from the campaign cache so the UI
	// can show "DROP 6" instead of stale "DROP 1" when Twitch's session
	// has advanced to a later drop in a multi-drop campaign.
	resolvedName := ""
	resolvedRequired := data.RequiredMinutesWatched
	s.mu.RLock()
	if c, ok := s.campaignCache[data.CampaignID]; ok {
		for _, d := range c.Drops {
			if d.ID == data.DropID {
				resolvedName = d.BenefitName
				if resolvedName == "" {
					resolvedName = d.Name
				}
				if resolvedRequired == 0 && d.RequiredMinutesWatched > 0 {
					resolvedRequired = d.RequiredMinutesWatched
				}
				break
			}
		}
	}
	s.mu.RUnlock()

	s.mu.Lock()
	updated := false
	for i := range s.activeDrops {
		if s.activeDrops[i].CampaignID != data.CampaignID {
			continue
		}
		// If the cached row already targets a different drop, swap to the new one.
		if data.DropID != "" && resolvedName != "" {
			s.activeDrops[i].DropName = resolvedName
		}
		if resolvedRequired > 0 {
			s.activeDrops[i].Required = resolvedRequired
		}
		s.activeDrops[i].Progress = data.CurrentMinutesWatched
		if s.activeDrops[i].Required > 0 {
			pct := (data.CurrentMinutesWatched * 100) / s.activeDrops[i].Required
			if pct > 100 {
				pct = 100
			}
			s.activeDrops[i].Percent = pct
			s.activeDrops[i].EtaMinutes = s.activeDrops[i].Required - data.CurrentMinutesWatched
			if s.activeDrops[i].EtaMinutes < 0 {
				s.activeDrops[i].EtaMinutes = 0
			}
		}
		updated = true
		break
	}
	// IMPORTANT: live WS/poll progress events must NOT touch the stall
	// baseline (StallTracker.SnapshotPick is the only writer). If we
	// shifted it forward here between cycles, healthy channels would
	// register as stalled at the next StallTracker.Apply.

	// Mark the timestamp so PollProgressOnce can skip its GQL call if a
	// fresh WS event already updated the same data (TDM
	// minute_almost_done).
	if updated {
		s.lastProgressUpdate = time.Now()
	}
	s.mu.Unlock()

	if updated {
		if s.writeLogFile != nil {
			s.writeLogFile(fmt.Sprintf("[Drops/WS] progress: campaign=%s drop=%s %d minutes",
				data.CampaignID, data.DropID, data.CurrentMinutesWatched))
		}

		// Mirror to picked channel's drop info so TUI shows the live
		// value. When the campaign advances to a new drop (e.g., drop 1
		// done → drop 2 starts), the resolved name/required from the
		// payload differ from the channel's previously-stored
		// snap.DropName/snap.DropRequired. Use the resolved values, not
		// the stale snap values, so the TUI / channel view stays in
		// sync with the activeDrops table.
		nextName := resolvedName
		nextRequired := resolvedRequired
		s.mu.RLock()
		pickedCh := s.currentPickID
		s.mu.RUnlock()
		if pickedCh != "" {
			if ch, ok := s.channels.Get(pickedCh); ok {
				snap := ch.Snapshot()
				if snap.HasActiveDrop {
					if nextName == "" {
						nextName = snap.DropName // fall back if cache miss
					}
					if nextRequired <= 0 {
						nextRequired = snap.DropRequired
					}
					ch.SetDropInfo(nextName, data.CurrentMinutesWatched, nextRequired)
				}
			}
		}
	}
}

// LookupCampaignByDropID searches the cached inventory for the campaign
// that owns the given drop ID. Returns "" if the drop is not in the
// current cache (e.g., a fresh inventory cycle hasn't run yet).
func (s *Service) LookupCampaignByDropID(dropID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.campaignCache {
		for _, d := range c.Drops {
			if d.ID == dropID {
				return c.ID
			}
		}
	}
	return ""
}
