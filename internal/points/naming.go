package points

// ResolveChannelName looks up a channel's display name by its ID for
// channels not in the registry. Used by the points/claim event handlers
// when PubSub fires for a channel the user watches but didn't add to
// the farmer (the user-level community-points-user-v1 topic delivers
// events for ALL channels, not just tracked ones).
//
// First-call performs a GQL lookup and caches the result; subsequent
// calls hit the cache. On lookup failure returns the raw channel ID so
// the log line still has SOMETHING readable rather than crashing or
// leaving the field empty.
func (s *Service) ResolveChannelName(channelID string) string {
	s.mu.RLock()
	if name, ok := s.nameCache[channelID]; ok {
		s.mu.RUnlock()
		return name
	}
	s.mu.RUnlock()

	name, err := s.gql.GetChannelNameByID(channelID)
	if err != nil || name == "" {
		return channelID
	}

	s.mu.Lock()
	s.nameCache[channelID] = name
	s.mu.Unlock()

	return name
}
