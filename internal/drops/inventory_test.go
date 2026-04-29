package drops

import (
	"errors"
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// stubClaimer captures ClaimDrop invocations. err lets a test simulate
// a failed claim path. ClaimDrop returning nil is "Twitch acknowledged
// the claim" (status `CLAIMED` or `ELIGIBLE_FOR_ALL` in production).
type stubClaimer struct {
	calls       []string
	returnError error
}

func (s *stubClaimer) ClaimDrop(dropInstanceID string) error {
	s.calls = append(s.calls, dropInstanceID)
	return s.returnError
}

func newServiceForClaimTest(cfg *config.Config) *Service {
	return &Service{
		cfg: cfg,
		log: func(string, ...interface{}) {},
	}
}

// TestAutoClaim_RegressionABILoop is the regression guard for the v2.0
// ABI Partner-Only loop:
//
// Pre-fix the bot ran the claim in a fire-and-forget goroutine. By the
// time ProcessDrops moved on to the Selector, the local IsClaimed flag
// was still false — the same campaign got picked again on a different
// allow-list channel, Twitch returned nil session for the freshly-
// claimed drop, and the bot blind-heartbeated through the whole 70+
// channel allow-list before the silent-pick threshold tripped.
//
// The fix has two pieces, both verified here:
//  1. AutoClaim claims synchronously and mutates `IsClaimed = true`
//     in-place on the campaigns slice.
//  2. The Selector's filterEligibleCampaigns sees the mutated state
//     and excludes the campaign from the next pick.
//
// If anyone reverts the in-place mutation (e.g. by re-introducing the
// `go func() { … }()` pattern) this test fails immediately.
func TestAutoClaim_RegressionABILoop(t *testing.T) {
	cfg := &config.Config{}
	s := newServiceForClaimTest(cfg)
	claimer := &stubClaimer{}

	campaigns := []twitch.DropCampaign{
		{
			ID: "abi-partner-only", Name: "ABI Partner-Only Drops",
			Status: "ACTIVE", IsAccountConnected: true,
			GameName: "Arena Breakout: Infinite",
			EndAt:    time.Now().Add(2 * time.Hour),
			Drops: []twitch.TimeBasedDrop{{
				ID:                     "drop-1",
				Name:                   "Infinite Memory Skin",
				RequiredMinutesWatched: 240,
				CurrentMinutesWatched:  240, // IsComplete() == true
				DropInstanceID:         "instance-abc",
				IsClaimed:              false,
				BenefitName:            "Infinite Memory Skin",
			}},
		},
	}

	s.autoClaimWith(campaigns, claimer)

	// 1. Claim was actually made with the right instance ID.
	if len(claimer.calls) != 1 {
		t.Fatalf("expected 1 ClaimDrop call, got %d", len(claimer.calls))
	}
	if claimer.calls[0] != "instance-abc" {
		t.Errorf("ClaimDrop got %q, want %q", claimer.calls[0], "instance-abc")
	}

	// 2. Slice was mutated in-place — IsClaimed flipped to true.
	if !campaigns[0].Drops[0].IsClaimed {
		t.Fatal("AutoClaim did not mutate IsClaimed in place — selector will re-pick the campaign and loop")
	}

	// 3. Selector now skips the campaign.
	sel := newTestSelector(cfg)
	out := sel.filterEligibleCampaigns(campaigns)
	if len(out) != 0 {
		t.Fatalf("post-claim, ABI campaign must be excluded from selector eligibility, got %d eligible campaigns", len(out))
	}
}

// TestAutoClaim_FailedClaimDoesNotMutate guards the inverse: a failed
// ClaimDrop must NOT flip IsClaimed locally — otherwise we'd silently
// drop a real failure and the campaign would be marked completed
// despite the user not having received the benefit.
func TestAutoClaim_FailedClaimDoesNotMutate(t *testing.T) {
	cfg := &config.Config{}
	s := newServiceForClaimTest(cfg)
	claimer := &stubClaimer{returnError: errors.New("network blew up")}

	campaigns := []twitch.DropCampaign{
		{
			ID: "abi", Status: "ACTIVE", IsAccountConnected: true,
			EndAt: time.Now().Add(2 * time.Hour),
			Drops: []twitch.TimeBasedDrop{{
				ID:                     "drop-1",
				RequiredMinutesWatched: 240,
				CurrentMinutesWatched:  240,
				DropInstanceID:         "instance-fail",
				IsClaimed:              false,
				BenefitName:            "Skin",
			}},
		},
	}

	s.autoClaimWith(campaigns, claimer)

	if campaigns[0].Drops[0].IsClaimed {
		t.Fatal("AutoClaim flipped IsClaimed despite ClaimDrop returning an error — must only mutate on success")
	}

	// Selector should still treat the campaign as eligible (drop is
	// IsComplete + unclaimed; the next cycle will retry the claim).
	sel := newTestSelector(cfg)
	out := sel.filterEligibleCampaigns(campaigns)
	if len(out) != 1 {
		t.Fatalf("failed-claim campaign must remain eligible for retry, got %d eligible", len(out))
	}
}

// TestAutoClaim_MultiDropChainAdvances simulates the Tarkov-Arena
// style multi-stage campaign: drop #1 hits 100%, gets claimed; drop #2
// is still unclaimed (and at 0/N, not complete). After AutoClaim:
//
//   - drop #1 is mutated to IsClaimed=true
//   - drop #2 stays unclaimed
//   - Selector still considers the campaign eligible (drop #2 is the
//     next earnable drop in the chain)
//
// This is the "don't false-cooldown a healthy multi-stage campaign"
// guard — V1 in our prior discussion.
func TestAutoClaim_MultiDropChainAdvances(t *testing.T) {
	cfg := &config.Config{}
	s := newServiceForClaimTest(cfg)
	claimer := &stubClaimer{}

	campaigns := []twitch.DropCampaign{
		{
			ID: "tarkov-arena-chain", Status: "ACTIVE", IsAccountConnected: true,
			GameName: "Escape from Tarkov: Arena",
			EndAt:    time.Now().Add(8 * time.Hour),
			Drops: []twitch.TimeBasedDrop{
				{
					ID:                     "drop-1",
					RequiredMinutesWatched: 120,
					CurrentMinutesWatched:  120, // complete
					DropInstanceID:         "instance-1",
					IsClaimed:              false,
					BenefitName:            "Stage 1",
				},
				{
					ID:                     "drop-2",
					RequiredMinutesWatched: 240,
					CurrentMinutesWatched:  0, // not started
					IsClaimed:              false,
					BenefitName:            "Stage 2",
				},
			},
		},
	}

	s.autoClaimWith(campaigns, claimer)

	if !campaigns[0].Drops[0].IsClaimed {
		t.Error("drop #1 should be claimed after AutoClaim")
	}
	if campaigns[0].Drops[1].IsClaimed {
		t.Error("drop #2 must NOT be claimed — it isn't complete yet")
	}

	sel := newTestSelector(cfg)
	out := sel.filterEligibleCampaigns(campaigns)
	if len(out) != 1 {
		t.Fatalf("multi-stage campaign with drop #2 still earnable must remain eligible, got %d", len(out))
	}
}
