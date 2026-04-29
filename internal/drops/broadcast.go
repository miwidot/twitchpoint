package drops

import "fmt"

// SubscribeBroadcastSettings subscribes to broadcast-settings-update for
// one channel. Used when a pick takes ownership of a channel so the
// service can react to mid-stream game changes (handleChannelGameChange).
func (s *Service) SubscribeBroadcastSettings(channelID string) {
	if s.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := s.pubsub.Listen([]string{topic}); err != nil {
		s.log("[PubSub] subscribe %s failed: %v", topic, err)
	}
}

// UnsubscribeBroadcastSettings drops the broadcast-settings-update topic
// for one channel. Used when releasing a previous pick.
func (s *Service) UnsubscribeBroadcastSettings(channelID string) {
	if s.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := s.pubsub.Unlisten([]string{topic}); err != nil {
		s.log("[PubSub] unsubscribe %s failed: %v", topic, err)
	}
}

// CleanupNonPickedTemps removes every temporary channel that is NOT the
// current pick. Drops doesn't own the full teardown (prober + IRC live
// in farmer), so the actual removal goes through the RemoveTempChannel
// callback supplied at construction.
func (s *Service) CleanupNonPickedTemps(pick *PoolEntry) {
	if s.removeTempChannel == nil {
		return
	}
	pickID := ""
	if pick != nil {
		pickID = pick.ChannelID
	}

	var stale []string
	for _, ch := range s.channels.States() {
		snap := ch.Snapshot()
		if snap.IsTemporary && snap.ChannelID != pickID {
			stale = append(stale, snap.ChannelID)
		}
	}

	for _, chID := range stale {
		s.removeTempChannel(chID)
	}
}
