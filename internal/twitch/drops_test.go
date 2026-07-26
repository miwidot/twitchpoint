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

// TestApplyClaimedBenefitFallback_ZeroedCounterAfterAward: Twitch zeroes the
// campaign counter after auto-granting a reward, so a fully farmed drop reports
// watched=0 with isClaimed=false. The in-window award is the only remaining
// evidence that the reward is already owned — guard 2 must not swallow it, or
// the bot re-farms an already-owned campaign forever (progress stuck at 0/0).
func TestApplyClaimedBenefitFallback_ZeroedCounterAfterAward(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "unique-badge", RequiredMinutesWatched: 900, CurrentMinutesWatched: 0},
		},
	}
	claimed := map[string]time.Time{"unique-badge": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	if !c.Drops[0].IsClaimed {
		t.Fatal("zeroed-counter drop with in-window award should be marked claimed")
	}
}

// TestApplyClaimedBenefitFallback_NeverWatchedNoAward: a never-watched drop
// (watched=0) whose benefit has NO award must stay open — the watched==0
// exemption must not mark drops the account doesn't own. Guards the case of a
// partially-farmed campaign whose later tiers aren't earned yet.
func TestApplyClaimedBenefitFallback_NeverWatchedNoAward(t *testing.T) {
	c := &DropCampaign{
		StartAt: campStart,
		EndAt:   campEnd,
		Drops: []TimeBasedDrop{
			{ID: "d1", BenefitID: "earned-badge", RequiredMinutesWatched: 60, CurrentMinutesWatched: 0},
			{ID: "d2", BenefitID: "unearned-badge", RequiredMinutesWatched: 900, CurrentMinutesWatched: 0},
		},
	}
	// Only d1's benefit was awarded; d2 has no award entry.
	claimed := map[string]time.Time{"earned-badge": awardInWin}

	applyClaimedBenefitFallback(c, claimed)

	if !c.Drops[0].IsClaimed {
		t.Fatal("d1 (awarded in-window) should be marked claimed")
	}
	if c.Drops[1].IsClaimed {
		t.Fatal("d2 (no award) must stay open so the campaign keeps farming")
	}
}
