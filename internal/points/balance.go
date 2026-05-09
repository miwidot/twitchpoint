package points

import "time"

// balanceRefreshInterval is how often we re-fetch each channel's
// points balance + (for online channels) stream metadata. 5 min keeps
// the dashboard reasonably fresh without hammering the GQL endpoint;
// PubSub PointsEarned events update the balance in near-real-time
// between refreshes.
const balanceRefreshInterval = 5 * time.Minute

// BalanceRefreshLoop ticks every balanceRefreshInterval and walks every
// tracked channel's balance + stream-metadata refresh. Started by
// Farmer.Start as a goroutine.
func (s *Service) BalanceRefreshLoop(stopCh <-chan struct{}) {
	ticker := time.NewTicker(balanceRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.RefreshBalances()
		case <-stopCh:
			return
		}
	}
}

// RefreshBalances iterates every tracked channel: fetches the channel-
// points balance, and for online channels also re-fetches stream
// metadata so the rotation has fresh broadcast IDs/game IDs to work
// with on the next tick.
//
// 500 ms inter-channel sleep keeps us under any informal rate-limit
// the GQL endpoint enforces — we'd otherwise burst N requests in
// roughly the same millisecond and risk a 429.
func (s *Service) RefreshBalances() {
	for _, ch := range s.channels.States() {
		balance, err := s.gql.GetChannelPointsBalance(ch.Login)
		if err == nil && balance > 0 {
			ch.SetBalance(balance)
		}

		snap := ch.Snapshot()
		if snap.IsOnline {
			info, err := s.gql.GetChannelInfo(ch.Login)
			if err == nil && info.IsLive {
				ch.SetOnlineWithGameID(info.BroadcastID, info.GameName, info.GameID, info.ViewerCount, info.StreamCreatedAt)
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}
