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
