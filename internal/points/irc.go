package points

// NotifyChannelAdded is called when a channel (permanent or temp) joins
// the registry. It hands the login to the IRC client for viewer
// presence — Twitch tracks viewer-presence-by-IRC-join independently
// of the channel-points-WATCH heartbeats, so the join is what makes
// the user count toward the streamer's viewer count.
//
// No-op when IRC is disabled in config (s.irc == nil).
func (s *Service) NotifyChannelAdded(login string) {
	if s.irc == nil {
		return
	}
	s.irc.Join(login)
}

// NotifyChannelRemoved is the inverse — drops IRC presence when a
// channel is removed (RemoveChannelLive) or torn down as a temp
// (removeTemporaryChannel).
func (s *Service) NotifyChannelRemoved(login string) {
	if s.irc == nil {
		return
	}
	s.irc.Part(login)
}
