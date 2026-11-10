package drops

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

func rfCampaign(id string, drops ...twitch.TimeBasedDrop) twitch.DropCampaign {
	return twitch.DropCampaign{
		ID: id, Name: id, Status: "ACTIVE", IsAccountConnected: true,
		EndAt: testNow.Add(2 * time.Hour), Drops: drops,
	}
}

func rfDrop(benefit string, subs int) twitch.TimeBasedDrop {
	d := makeWatchableDrop()
	d.BenefitType = benefit
	d.RequiredSubs = subs
	return d
}

func TestRewardFilters(t *testing.T) {
	badge := rfCampaign("badge", rfDrop("BADGE", 0))
	emote := rfCampaign("emote", rfDrop("EMOTE", 0), rfDrop("BADGE", 0))
	mixed := rfCampaign("mixed", rfDrop("BADGE", 0), rfDrop("DIRECT_ENTITLEMENT", 0))
	item := rfCampaign("item", rfDrop("DIRECT_ENTITLEMENT", 0))
	unknown := rfCampaign("unknown", rfDrop("", 0))
	subbed := rfCampaign("subbed", rfDrop("DIRECT_ENTITLEMENT", 0), rfDrop("DIRECT_ENTITLEMENT", 5))
	flagged := rfCampaign("flagged", rfDrop("DIRECT_ENTITLEMENT", 0))
	flagged.HasSubReward = true
	all := []twitch.DropCampaign{badge, emote, mixed, item, unknown, subbed, flagged}

	ids := func(cfg *config.Config) map[string]bool {
		out := map[string]bool{}
		for _, c := range newTestSelector(cfg).filterEligibleCampaigns(all) {
			out[c.ID] = true
		}
		return out
	}

	t.Run("defaults keep everything", func(t *testing.T) {
		if got := ids(&config.Config{}); len(got) != len(all) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("skip badge/emote drops only badge-only campaigns", func(t *testing.T) {
		got := ids(&config.Config{SkipBadgeEmote: true})
		for _, id := range []string{"badge", "emote"} {
			if got[id] {
				t.Errorf("%s should be skipped", id)
			}
		}
		for _, id := range []string{"mixed", "item", "unknown", "subbed", "flagged"} {
			if !got[id] {
				t.Errorf("%s should be kept", id)
			}
		}
	})
	t.Run("skip sub rewards", func(t *testing.T) {
		got := ids(&config.Config{SkipSubRewards: true})
		for _, id := range []string{"subbed", "flagged"} {
			if got[id] {
				t.Errorf("%s should be skipped", id)
			}
		}
		if len(got) != 5 {
			t.Errorf("got %v", got)
		}
	})
}

func TestBuildRowsRewardFilterParity(t *testing.T) {
	cfg := &config.Config{SkipBadgeEmote: true}
	c := rfCampaign("badge", rfDrop("BADGE", 0))
	active, queued, idle := BuildRows(cfg, []twitch.DropCampaign{c}, nil, nil)
	if len(active)+len(queued) != 0 {
		t.Fatalf("filtered campaign must not be active/queued")
	}
	for _, r := range idle {
		if r.BlockReason == "" {
			t.Errorf("idle row should explain the skip")
		}
	}
}
