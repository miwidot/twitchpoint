package twitch

import (
	"testing"
	"time"
)

// campaignWindow is a fixed [start, end) used by the fallback tests.
var (
	campStart  = time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	campEnd    = time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	awardInWin = time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
)

// TestApplyClaimedBenefitFallback_SharedBenefitIncompleteNotClaimed is the
// GitHub issue #7 regression: a campaign whose drops all share ONE benefit
// ID at escalating watch tiers (R6S "Esports Pack 2026 S1.2"), none of them
// at 100%, must NOT be marked claimed just because that benefit was awarded
// once inside the campaign window. Before the fix, all tiers flipped to
// IsClaimed=true and the campaign was falsely marked fully completed.
func TestApplyClaimedBenefitFallback_SharedBenefitIncompleteNotClaimed(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "esports-pack", RequiredMinutesWatched: 540, CurrentMinutesWatched: 335}, // 62%
			{ID: "d2", BenefitID: "esports-pack", RequiredMinutesWatched: 720, CurrentMinutesWatched: 0},   // 0%
			{ID: "d3", BenefitID: "esports-pack", RequiredMinutesWatched: 540, CurrentMinutesWatched: 448}, // 83%
		},
	}
	claimed := map[string]time.Time{"esports-pack": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	for _, d := range c.Drops {
		if d.IsClaimed {
			t.Fatalf("drop %s (%d/%d) wrongly marked claimed by shared-benefit award",
				d.ID, d.CurrentMinutesWatched, d.RequiredMinutesWatched)
		}
	}
}

// TestApplyClaimedBenefitFallback_SharedBenefitAllCompleteStillAmbiguous:
// even when the shared-benefit tiers are ALL at 100%, a single award can't
// prove every tier was claimed, so the ambiguous signal is ignored (guard 3).
// Inventory's per-drop isClaimed remains the source of truth.
func TestApplyClaimedBenefitFallback_SharedBenefitAllCompleteStillAmbiguous(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "esports-pack", RequiredMinutesWatched: 540, CurrentMinutesWatched: 540},
			{ID: "d2", BenefitID: "esports-pack", RequiredMinutesWatched: 720, CurrentMinutesWatched: 720},
		},
	}
	claimed := map[string]time.Time{"esports-pack": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	for _, d := range c.Drops {
		if d.IsClaimed {
			t.Fatalf("drop %s wrongly marked claimed — one award can't cover multiple shared-benefit tiers", d.ID)
		}
	}
}

// TestApplyClaimedBenefitFallback_UniqueCompleteClaimed is the legitimate
// case the fallback exists for: a single unique-benefit drop at 100% whose
// benefit was awarded inside the window (dashboard just lagging isClaimed).
// It must be recognised as claimed.
func TestApplyClaimedBenefitFallback_UniqueCompleteClaimed(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "unique-badge", RequiredMinutesWatched: 60, CurrentMinutesWatched: 60},
		},
	}
	claimed := map[string]time.Time{"unique-badge": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	if !c.Drops[0].IsClaimed {
		t.Fatal("unique complete drop with in-window award should be marked claimed")
	}
}

// TestApplyClaimedBenefitFallback_UniqueIncompleteNotClaimed: a unique-benefit
// drop below 100% can't have been claimed even if the benefit shows an award
// (e.g. a stale/cross-campaign award) — guard 2.
func TestApplyClaimedBenefitFallback_UniqueIncompleteNotClaimed(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "unique-badge", RequiredMinutesWatched: 60, CurrentMinutesWatched: 30},
		},
	}
	claimed := map[string]time.Time{"unique-badge": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	if c.Drops[0].IsClaimed {
		t.Fatal("incomplete drop (30/60) must not be marked claimed")
	}
}

// TestApplyClaimedBenefitFallback_AwardOutsideWindow: a unique complete drop
// whose only award predates the campaign window must not be marked claimed
// (guard 1 — daily-rolling protection).
func TestApplyClaimedBenefitFallback_AwardOutsideWindow(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "unique-badge", RequiredMinutesWatched: 60, CurrentMinutesWatched: 60},
		},
	}
	claimed := map[string]time.Time{"unique-badge": campStart.Add(-48 * time.Hour)}

	applyClaimedBenefitFallback(c, claimed)

	if c.Drops[0].IsClaimed {
		t.Fatal("award before the campaign window must not mark the drop claimed")
	}
}
