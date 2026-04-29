package drops

import (
	"github.com/miwi/twitchpoint/internal/twitch"
)

// dropClaimer is the minimal GQL surface AutoClaim needs. Defined as
// an interface so unit tests can stub the claim call without spinning
// up a real Twitch GQL client. *twitch.GQLClient satisfies it via its
// existing ClaimDrop method.
type dropClaimer interface {
	ClaimDrop(dropInstanceID string) error
}

// AutoClaimAndMarkCompleted walks an inventory campaigns list and claims
// every complete-but-unclaimed drop instance synchronously, mutating
// the local IsClaimed flag in-place on success. When every watchable
// drop is claimed the campaign is marked completed in config.
//
// Synchronous claim + in-place mutation is intentional. The earlier
// goroutine-based version was fire-and-forget — by the time
// ProcessDrops moved on to Selector, ClaimDrop was still in flight and
// the local d.IsClaimed was still false, so the selector saw the
// campaign as eligible and re-picked the same channel for a drop that
// was already claimed (or about to be). Twitch's session API then
// returned nil and the bot blind-heartbeated until the silent-pick
// threshold tripped. TDM does the same in-place mutation
// (src/models/drop.py:149 — self.is_claimed = result).
//
// The slice is mutated through a pointer index loop. Callers passing
// the campaign list to subsequent stages (Selector, BuildRows,
// SnapshotPick) see the updated IsClaimed state without a second
// inventory fetch.
//
// !InInventory alone is NOT a completion signal here — that would
// false-positive on never-started campaigns. Use
// MarkCompletedIfFinishedExternally for the picked channel's
// "finished-while-watching" path; this method only marks completion
// when every watchable drop is observably claimed.
func (s *Service) AutoClaimAndMarkCompleted(campaigns []twitch.DropCampaign) {
	s.autoClaimWith(campaigns, s.gql)
}

// autoClaimWith is the testable inner. Tests pass a stub claimer
// (anything implementing dropClaimer) instead of the real GQL client.
func (s *Service) autoClaimWith(campaigns []twitch.DropCampaign, claimer dropClaimer) {
	for ci := range campaigns {
		c := &campaigns[ci]
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if s.cfg.IsCampaignCompleted(c.ID) {
			continue
		}

		allClaimed := true
		hasWatchable := false
		for di := range c.Drops {
			d := &c.Drops[di]
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			hasWatchable = true
			if d.IsClaimed {
				continue
			}
			if d.IsComplete() && d.DropInstanceID != "" {
				name := d.BenefitName
				if name == "" {
					name = d.Name
				}
				if err := claimer.ClaimDrop(d.DropInstanceID); err != nil {
					s.log("[Drops] Failed to claim %s: %v", name, err)
					allClaimed = false
				} else {
					s.log("[Drops] Claimed: %s (%s)", name, c.Name)
					// Mutate the slice's drop in-place so downstream
					// stages (Selector, SnapshotPick) see the fresh
					// claim without another inventory round-trip.
					d.IsClaimed = true
				}
			} else {
				// Drop is unclaimed AND not complete (or no instance
				// ID yet) — campaign isn't fully claimed.
				allClaimed = false
			}
		}

		if hasWatchable && allClaimed {
			s.cfg.MarkCampaignCompleted(c.ID)
			_ = s.cfg.Save()
			s.log("[Drops] Campaign %q fully claimed — marked as completed", c.Name)
		}
	}
}

// MarkCompletedIfFinishedExternally is called by the progress poller when
// the picked channel's drop is at 100%. It re-fetches inventory and
// confirms the campaign is no longer in dropCampaignsInProgress before
// marking it completed in config. The combo is reliable: poll says
// "Twitch credited me past required" + inventory says "campaign no
// longer in progress list" = genuinely done. Avoids the false-positive
// of marking never-started campaigns as completed just because they
// aren't in the inventory yet.
func (s *Service) MarkCompletedIfFinishedExternally(campaignID string) {
	campaigns, err := s.gql.GetDropsInventory()
	if err != nil {
		return
	}
	for _, c := range campaigns {
		if c.ID != campaignID {
			continue
		}
		if c.InInventory {
			// Still in progress — has more drops to farm. Don't mark.
			return
		}
		if s.cfg.IsCampaignCompleted(c.ID) {
			return
		}
		s.cfg.MarkCampaignCompleted(c.ID)
		_ = s.cfg.Save()
		s.log("[Drops] Campaign %q finished externally (poll: complete + not in inventory) — marked completed", c.Name)
		return
	}
}
