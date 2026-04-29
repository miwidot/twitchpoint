package drops

import (
	"fmt"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// ApplyPick registers the picked channel as a temp channel if not
// already tracked, sets HasActiveDrop=true on it, and clears
// HasActiveDrop on any other channel that was the previous pick.
//
// Returns true on success (state mutated, Watcher started OR pick==nil
// cleared state cleanly). Returns false when the metadata refresh
// failed for a non-nil pick — callers must NOT commit CurrentPickID
// in that case, otherwise we end up with state believing "drop is
// running" while no Watcher is active.
func (s *Service) ApplyPick(pick *PoolEntry, campaigns []twitch.DropCampaign) bool {
	s.RLock()
	prevPickID := s.CurrentPickID
	s.RUnlock()

	if pick == nil {
		if prevPickID != "" {
			if ch, ok := s.channels.Get(prevPickID); ok {
				ch.ClearDropInfo()
				// Clear IsWatching so rotation can pick this channel up
				// again as a normal Spade slot.
				ch.SetWatching(false)
			}
			s.UnsubscribeBroadcastSettings(prevPickID)
		}
		if s.watcher != nil {
			s.watcher.Stop()
		}
		return true
	}

	// 1. SINGLE source of truth for metadata: fetch upfront BEFORE any
	//    state mutation. If this fails, NO channel is added, NO drop
	//    info changes, NO previous pick is released, NO topics are
	//    subscribed. The next cycle retries cleanly with the existing
	//    pick still in effect.
	info, err := s.gql.GetChannelInfo(pick.ChannelLogin)
	if err != nil || info == nil || !info.IsLive || info.BroadcastID == "" || info.GameID == "" {
		liveStr := "false"
		bidStr := ""
		gidStr := ""
		if info != nil {
			if info.IsLive {
				liveStr = "true"
			}
			bidStr = info.BroadcastID
			gidStr = info.GameID
		}
		s.log("[Drops/Watch] skip %s — refresh failed (live=%s broadcast=%q game_id=%q)",
			pick.ChannelLogin, liveStr, bidStr, gidStr)
		return false
	}

	// 2. Game-match guard: streamer may have switched games between
	//    selector run and now. If the freshly-fetched game doesn't
	//    match any of the pick's campaigns, abort — sending
	//    sendSpadeEvents with a wrong game_id makes Twitch silently
	//    drop credit.
	if !PickGameMatches(pick, info.GameName) {
		s.log("[Drops/Watch] skip %s — game changed to %q (expected one of %s)",
			pick.ChannelLogin, info.GameName, PickCampaignGames(pick))
		// Manual-reason cooldown so the selector doesn't immediately re-pick.
		s.Stall.SetManual(pick.ChannelID, 15*time.Minute)
		return false
	}

	// 3. Channel-ID consistency: pick.ChannelID came from the selector
	//    pool (built from directory or allowed_channels). info.ID came
	//    from a direct user(login:) lookup just now. They MUST match —
	//    if they don't, our internal channels[] map (keyed by ChannelID)
	//    will get confused (e.g., create a temp with info.ID but later
	//    look it up with pick.ChannelID and miss it, leaving an orphaned
	//    temp).
	if info.ID != pick.ChannelID {
		s.log("[Drops/Watch] skip %s — id mismatch (pick=%s info=%s) — cooldown",
			pick.ChannelLogin, pick.ChannelID, info.ID)
		// Cooldown the broken pool ID so the selector doesn't immediately
		// re-pick the same wrong entry next cycle.
		s.Stall.SetManual(pick.ChannelID, 30*time.Minute)
		return false
	}

	// 4. Resolve or create channel state, using the already-fetched info.
	//    No second GetChannelInfo call — same data drives temp creation
	//    AND watcher start, so we can't end up with a registered temp
	//    that failed its refresh.
	ch, exists := s.channels.Get(pick.ChannelID)
	if !exists {
		primaryCampID := ""
		if len(pick.Campaigns) > 0 {
			primaryCampID = pick.Campaigns[0].ID
		}
		if err := s.addTempChannelFromInfo(info, primaryCampID); err != nil {
			s.log("[Drops/Pool] failed to add %s: %v", pick.ChannelLogin, err)
			return false
		}
		ch, exists = s.channels.Get(pick.ChannelID)
		if !exists {
			return false
		}
	} else {
		// Existing channel — refresh its state with the verified metadata.
		ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
	}
	snap := ch.Snapshot()

	// 5. Metadata is valid — NOW it's safe to mutate state.
	primaryCampID := ""
	if len(pick.Campaigns) > 0 {
		primaryCampID = pick.Campaigns[0].ID
	}
	for _, c := range campaigns {
		if c.ID != primaryCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
				continue
			}
			name := d.BenefitName
			if name == "" {
				name = d.Name
			}
			ch.SetDropInfo(name, d.CurrentMinutesWatched, d.RequiredMinutesWatched)
			ch.SetCampaignID(c.ID)
			break
		}
		break
	}

	// 6. Release previous pick (only if it's a different channel).
	if prevPickID != "" && prevPickID != pick.ChannelID {
		if prevCh, ok := s.channels.Get(prevPickID); ok {
			prevCh.ClearDropInfo()
			prevCh.SetWatching(false)
		}
		s.UnsubscribeBroadcastSettings(prevPickID)
	}

	// 7. Subscribe broadcast-settings-update for the new pick.
	s.SubscribeBroadcastSettings(pick.ChannelID)

	// 8. Hand the channel to the drops Watcher.
	if s.watcher != nil {
		s.spade.StopWatching(snap.ChannelID)
		s.prober.Stop(snap.Login)
		ch.SetWatching(true) // for UI display
		s.watcher.Start(snap.ChannelID, snap.Login, snap.BroadcastID, snap.GameName, snap.GameID)
		s.log("[Drops/Watch] handing %s to drops Watcher (exclusive)", snap.DisplayName)
	}
	return true
}

// RefreshWatcherBroadcast fetches the channel's current stream metadata
// and pushes it into the running drops Watcher. Used when
// broadcast-settings-update fires for the currently-picked channel and
// the streamer is still on the expected game — Twitch may have issued a
// new broadcast_id mid-session (stream restart, title change, etc.) and
// the Watcher must use the new one in subsequent sendSpadeEvents
// heartbeats.
func (s *Service) RefreshWatcherBroadcast(channelID, login string) {
	if s.watcher == nil {
		return
	}
	info, err := s.gql.GetChannelInfo(login)
	if err != nil || info == nil || !info.IsLive {
		return
	}
	// Don't push empty IDs into the Watcher. GetChannelInfo can
	// momentarily return IsLive=true with an empty broadcast_id during
	// a stream-restart transition; the Watcher would then send
	// heartbeats with broadcast_id="" until the next refresh.
	if info.BroadcastID == "" || info.GameID == "" {
		return
	}
	if ch, ok := s.channels.Get(channelID); ok {
		ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount)
	}
	s.watcher.UpdateBroadcast(channelID, info.BroadcastID, info.GameName, info.GameID)
}

// HandleGameChange reacts to a broadcast-settings-update PubSub event.
// If the channel was the current drop pick AND the new game does not
// match the picked campaign's game, the channel is added to the stall
// cooldown for 15 min and an out-of-cycle selector re-run is triggered.
//
// Per TDM message_handlers.py:121 (check_online → ONLINE_DELAY 120s):
// debounce 30s before reacting. Streamers often flap game/title rapidly
// (especially during stream-start or category transitions), and
// reacting instantly causes unnecessary channel switches. After 30s,
// re-fetch the channel's actual current game; if the streamer has
// switched back to the expected game by then, no action.
func (s *Service) HandleGameChange(channelID string, data twitch.GameChangeData) {
	s.RLock()
	currentPick := s.CurrentPickID
	s.RUnlock()
	pickCampaign := s.Stall.LastPickCampaignID()

	if channelID != currentPick {
		if data.OldGameName != data.NewGameName && s.writeLogFile != nil {
			s.writeLogFile(fmt.Sprintf("[Drops/WS] non-pick channel %s game changed: %s -> %s",
				channelID, data.OldGameName, data.NewGameName))
		}
		return
	}

	s.RLock()
	expectedGame := ""
	pickedChannelLogin := ""
	if c, ok := s.CampaignCache[pickCampaign]; ok {
		expectedGame = c.GameName
	}
	if ch, ok := s.channels.Get(channelID); ok {
		pickedChannelLogin = ch.Login
	}
	s.RUnlock()

	if expectedGame == "" || pickedChannelLogin == "" {
		return
	}

	// Same-game broadcast-settings-update events ALSO need to refresh
	// the Watcher — the streamer may have restarted the broadcast (new
	// broadcast_id with same game) or changed title/tags. Without this
	// refresh, the Watcher keeps sending the old broadcast_id and
	// Twitch silently drops credit until the next pick cycle.
	if data.OldGameName == data.NewGameName {
		go s.RefreshWatcherBroadcast(channelID, pickedChannelLogin)
		return
	}

	// Optimistic early-out: payload already shows we're back on the right game.
	if strings.EqualFold(data.NewGameName, expectedGame) {
		s.log("[Drops/WS] %s switched back to %q — keeping pick", channelID, expectedGame)
		go s.RefreshWatcherBroadcast(channelID, pickedChannelLogin)
		return
	}

	// Debounce 30s, then re-verify via fresh GetChannelInfo before
	// applying the cooldown. Absorbs streamer flapping.
	go func() {
		time.Sleep(30 * time.Second)

		// Re-check whether this is still the picked channel (selector
		// may have moved on while we slept).
		s.RLock()
		stillPicked := s.CurrentPickID == channelID
		s.RUnlock()
		if !stillPicked {
			return
		}

		info, err := s.gql.GetChannelInfo(pickedChannelLogin)
		if err == nil && strings.EqualFold(info.GameName, expectedGame) {
			s.log("[Drops/WS] %s flapped back to %q during 30s debounce — keeping pick",
				channelID, expectedGame)
			return
		}

		s.Stall.SetManual(channelID, 15*time.Minute)

		s.log("[Drops/WS] %s changed game (%s -> %s); still wrong after 30s — 15min cooldown, re-picking",
			channelID, data.OldGameName, data.NewGameName)

		s.ProcessDrops()
	}()
}
