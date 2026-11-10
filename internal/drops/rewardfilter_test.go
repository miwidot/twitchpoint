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

func TestSortPool_BadgeEmoteLast(t *testing.T) {
	badge := rfCampaign("badge", rfDrop("BADGE", 0))
	badge.EndAt = testNow.Add(1 * time.Hour) // ends sooner: would win without the flag
	item := rfCampaign("item", rfDrop("DIRECT_ENTITLEMENT", 0))
	item.EndAt = testNow.Add(5 * time.Hour)
	ref := func(c twitch.DropCampaign) CampaignRef {
		return CampaignRef{ID: c.ID, GameName: "g", EndAt: c.EndAt, BadgeEmoteOnly: isBadgeEmoteOnly(c)}
	}
	mk := func() []*PoolEntry {
		return []*PoolEntry{
			{ChannelID: "b", ViewerCount: 900, Campaigns: []CampaignRef{ref(badge)}},
			{ChannelID: "i", ViewerCount: 10, Campaigns: []CampaignRef{ref(item)}},
		}
	}
	off := mk()
	newTestSelector(&config.Config{}).sortPool(off)
	if off[0].ChannelID != "b" {
		t.Fatalf("baseline expected badge first, got %s", off[0].ChannelID)
	}
	on := mk()
	newTestSelector(&config.Config{BadgeEmoteLast: true}).sortPool(on)
	if on[0].ChannelID != "i" || on[1].ChannelID != "b" {
		t.Fatalf("badge-last: got %s,%s", on[0].ChannelID, on[1].ChannelID)
	}
	// Only badge channels available -> still picked (fallback works).
	only := []*PoolEntry{mk()[0]}
	newTestSelector(&config.Config{BadgeEmoteLast: true}).sortPool(only)
	if len(only) != 1 || only[0].ChannelID != "b" {
		t.Fatal("fallback to badge channel lost")
	}
}
