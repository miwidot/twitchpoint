package drops

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

func completeDrop(benefitType string) twitch.TimeBasedDrop {
	return twitch.TimeBasedDrop{
		ID: "drop-1", Name: "Mouseathon Badge", BenefitType: benefitType,
		RequiredMinutesWatched: 15, CurrentMinutesWatched: 15, DropInstanceID: "instance-1",
	}
}

func TestAutoClaim_BadgeCampaignWithoutAccountLink(t *testing.T) {
	s := newServiceForClaimTest(&config.Config{AutoClaim: true})
	claimer := &stubClaimer{}
	campaigns := []twitch.DropCampaign{{
		ID: "subathon", Status: "ACTIVE", IsAccountConnected: false,
		EndAt: time.Now().Add(time.Hour), Drops: []twitch.TimeBasedDrop{completeDrop("BADGE")},
	}}

	s.autoClaimWith(campaigns, claimer)

	if len(claimer.calls) != 1 {
		t.Fatalf("badge drop of an unlinked campaign not claimed (calls=%v)", claimer.calls)
	}
}

func TestAutoClaim_GameRewardStillNeedsAccountLink(t *testing.T) {
	s := newServiceForClaimTest(&config.Config{AutoClaim: true})
	claimer := &stubClaimer{}
	campaigns := []twitch.DropCampaign{{
		ID: "game-reward", Status: "ACTIVE", IsAccountConnected: false,
		EndAt: time.Now().Add(time.Hour), Drops: []twitch.TimeBasedDrop{completeDrop("DIRECT_ENTITLEMENT")},
	}}

	s.autoClaimWith(campaigns, claimer)

	if len(claimer.calls) != 0 {
		t.Fatalf("unlinked game-reward campaign was claimed (calls=%v)", claimer.calls)
	}
}

// A campaign mixing a badge with a game item, still unlinked, is the case the
// campaign-level gate alone gets wrong: hasBadgeOrEmoteBenefit is an ANY-match,
// so the campaign passes it. Only the badge may be claimed, the game item has
// to wait for the account link, and the campaign must stay open.
func TestAutoClaim_MixedUnlinkedCampaignClaimsOnlyTheBadge(t *testing.T) {
	cfg := &config.Config{AutoClaim: true}
	s := newServiceForClaimTest(cfg)
	claimer := &stubClaimer{}
	campaigns := []twitch.DropCampaign{{
		ID: "mixed", Status: "ACTIVE", IsAccountConnected: false,
		EndAt: time.Now().Add(time.Hour),
		Drops: []twitch.TimeBasedDrop{
			{ID: "d-badge", Name: "Badge", BenefitType: "BADGE",
				RequiredMinutesWatched: 15, CurrentMinutesWatched: 15, DropInstanceID: "i-badge"},
			{ID: "d-game", Name: "Game Item", BenefitType: "DIRECT_ENTITLEMENT",
				RequiredMinutesWatched: 30, CurrentMinutesWatched: 30, DropInstanceID: "i-game"},
		},
	}}

	s.autoClaimWith(campaigns, claimer)

	if len(claimer.calls) != 1 || claimer.calls[0] != "i-badge" {
		t.Fatalf("expected only the badge to be claimed, got calls=%v", claimer.calls)
	}
	if cfg.IsCampaignCompleted("mixed") {
		t.Fatal("campaign marked completed while the game item is still unclaimed")
	}
}
