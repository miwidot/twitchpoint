package drops

import (
	"github.com/miwi/twitchpoint/internal/twitch"
)

// AutoClaimAndMarkCompleted walks an inventory campaigns list and claims
// every complete-but-unclaimed drop instance asynchronously. When a
// campaign's watchable drops are all claimed, the campaign is marked
// completed in config and persisted.
//
// !InInventory alone is NOT a completion signal here — that would
// false-positive on never-started campaigns. Use
// MarkCompletedIfFinishedExternally for the picked channel's
// "finished-while-watching" path; this method only marks completion
// when every watchable drop is observably claimed.
func (s *Service) AutoClaimAndMarkCompleted(campaigns []twitch.DropCampaign) {
	for _, c := range campaigns {
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
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			hasWatchable = true
			if d.IsClaimed {
				continue
			}
			allClaimed = false
			if d.IsComplete() && d.DropInstanceID != "" {
				name := d.BenefitName
				if name == "" {
					name = d.Name
				}
				instanceID := d.DropInstanceID
				dropName := name
				campaignName := c.Name
				go func() {
					if err := s.gql.ClaimDrop(instanceID); err != nil {
						s.log("[Drops] Failed to claim %s: %v", dropName, err)
					} else {
						s.log("[Drops] Claimed: %s (%s)", dropName, campaignName)
					}
				}()
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
