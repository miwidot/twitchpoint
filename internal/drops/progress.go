package drops

import (
	"fmt"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

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
	s.RLock()
	pickedID := s.CurrentPickID
	recentUpdate := !s.LastProgressUpdate.IsZero() && time.Since(s.LastProgressUpdate) < 30*time.Second
	s.RUnlock()
	pickedCampID := s.Stall.LastPickCampaignID()

	if pickedID == "" || pickedCampID == "" {
		return
	}
	if recentUpdate {
		return
	}

	session, err := s.gql.GetCurrentDropSession(pickedID)
	if err != nil {
		// Silent — this happens during stream offline/transition; not worth a log.
		return
	}
	if session == nil {
		// Twitch reports no active drop session for this channel. Could
		// mean the streamer's drops aren't crediting us — let the stall
		// cooldown handle it.
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

	s.RLock()
	pickedID := s.CurrentPickID
	s.RUnlock()

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
	s.RLock()
	if c, ok := s.CampaignCache[data.CampaignID]; ok {
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
	s.RUnlock()

	s.Lock()
	updated := false
	for i := range s.ActiveDrops {
		if s.ActiveDrops[i].CampaignID != data.CampaignID {
			continue
		}
		// If the cached row already targets a different drop, swap to the new one.
		if data.DropID != "" && resolvedName != "" {
			s.ActiveDrops[i].DropName = resolvedName
		}
		if resolvedRequired > 0 {
			s.ActiveDrops[i].Required = resolvedRequired
		}
		s.ActiveDrops[i].Progress = data.CurrentMinutesWatched
		if s.ActiveDrops[i].Required > 0 {
			pct := (data.CurrentMinutesWatched * 100) / s.ActiveDrops[i].Required
			if pct > 100 {
				pct = 100
			}
			s.ActiveDrops[i].Percent = pct
			s.ActiveDrops[i].EtaMinutes = s.ActiveDrops[i].Required - data.CurrentMinutesWatched
			if s.ActiveDrops[i].EtaMinutes < 0 {
				s.ActiveDrops[i].EtaMinutes = 0
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
		s.LastProgressUpdate = time.Now()
	}
	s.Unlock()

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
		s.RLock()
		pickedCh := s.CurrentPickID
		s.RUnlock()
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
	s.RLock()
	defer s.RUnlock()
	for _, c := range s.CampaignCache {
		for _, d := range c.Drops {
			if d.ID == dropID {
				return c.ID
			}
		}
	}
	return ""
}
