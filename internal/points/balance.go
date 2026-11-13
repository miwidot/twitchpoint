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
	states := s.channels.States()

	// Balance failures are counted and reported once per pass instead of
	// per channel. With ~40 channels the per-channel line would bury
	// everything else in the log, and the cause is nearly always shared by
	// all of them (dead token, schema change, endpoint down) — so one
	// summary carries the same information.
	var failed int
	var lastErr error

	for _, ch := range states {
		balance, err := s.gql.GetChannelPointsBalance(ch.Login)
		if err != nil {
			failed++
			lastErr = err
			s.debugLog("balance refresh failed for %s: %v", ch.Login, err)
		} else {
			// Set even when zero — a genuine zero is a fact worth showing.
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

	if failed > 0 {
		s.log("Balance refresh: %d of %d channels failed, last error: %v",
			failed, len(states), lastErr)
	}
}
