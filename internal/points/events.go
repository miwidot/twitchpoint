package points

import (
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
)

// dedupTTL is how long we remember claim/raid IDs before pruning.
// PubSub re-fires EventClaimAvailable every few seconds while the bonus
// is pending and EventRaid every second during the raid countdown, so a
// short TTL is fine — once the dedup window closes the original event
// is long gone from the wire.
const dedupTTL = 30 * time.Minute

// SeenClaim returns true if the claim was already attempted in this
// dedup window. Otherwise it records the claim ID, prunes expired
// entries, and returns false. Callers should bail out on true to avoid
// double-claim retries (each attempt is 3× retried, so a missed dedup
// triples the API load on already-claimed bonuses).
func (s *Service) SeenClaim(claimID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.seenClaims[claimID]; seen {
		return true
	}
	s.seenClaims[claimID] = time.Now()
	for id, t := range s.seenClaims {
		if time.Since(t) > dedupTTL {
			delete(s.seenClaims, id)
		}
	}
	return false
}

// SeenRaid returns true if the raid was already attempted. Twitch
// fires EventRaid every second during the countdown so dedup is
// load-bearing; without it we'd JoinRaid 30+ times for a single raid.
func (s *Service) SeenRaid(raidID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.seenRaids[raidID]; seen {
		return true
	}
	s.seenRaids[raidID] = time.Now()
	for id, t := range s.seenRaids {
		if time.Since(t) > dedupTTL {
			delete(s.seenRaids, id)
		}
	}
	return false
}

// RecordPoints adds to the running totalPointsEarned counter. Called by
// the EventPointsEarned handler for both tracked and untracked channels
// (untracked channels still credit globally; per-channel session totals
// only update when the channel is in the registry).
func (s *Service) RecordPoints(gained int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalPointsEarned += gained
}

// AttemptClaim runs the channel-points bonus claim flow asynchronously
// with up to 3 retries (2s spaced). On success it bumps the running
// total, records the claim against the channel state if non-nil, and
// logs the success line. On all-3-failed it logs the failure with the
// last error.
//
// Spawns a goroutine internally — handleEvent must NOT block on
// network calls or it'll back up the PubSub event channel.
func (s *Service) AttemptClaim(channelID, claimID, channelName string, ch *channels.State) {
	go func() {
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(2 * time.Second)
			}
			lastErr = s.gql.ClaimCommunityPoints(channelID, claimID)
			if lastErr == nil {
				if ch != nil {
					ch.RecordClaim()
				}
				s.mu.Lock()
				s.totalClaimsMade++
				s.mu.Unlock()
				s.log("Claimed bonus on %s!", channelName)
				return
			}
		}
		s.log("Claim failed on %s after 3 attempts: %v", channelName, lastErr)
	}()
}
