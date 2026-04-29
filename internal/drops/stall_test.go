package drops

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

func makeCampaign(id string, dropID string, current, required int, claimed bool) twitch.DropCampaign {
	return twitch.DropCampaign{
		ID: id,
		Drops: []twitch.TimeBasedDrop{{
			ID:                     dropID,
			RequiredMinutesWatched: required,
			CurrentMinutesWatched:  current,
			IsClaimed:              claimed,
		}},
	}
}

func TestStallTracker_NoBaseline_NoCooldown(t *testing.T) {
	s := NewStallTracker(nil)
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	if got := s.ActiveSkipSet(); len(got) != 0 {
		t.Errorf("Apply with no baseline left %d cooldowns, want 0", len(got))
	}
}

func TestStallTracker_ProgressAdvanced_NoCooldown(t *testing.T) {
	s := NewStallTracker(nil)
	pick := &PoolEntry{ChannelID: "ch1", Campaigns: []CampaignRef{{ID: "c1"}}}
	s.SnapshotPick(pick, []twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	// Next cycle: progress went 5 → 7
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 7, 60, false)})
	if _, in := s.ActiveSkipSet()["ch1"]; in {
		t.Error("ch1 was put in cooldown despite progress advancing")
	}
}

func TestStallTracker_ProgressStalled_AddsCooldown(t *testing.T) {
	s := NewStallTracker(nil)
	pick := &PoolEntry{ChannelID: "ch1", Campaigns: []CampaignRef{{ID: "c1"}}}
	s.SnapshotPick(pick, []twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	// Next cycle: still 5
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	if _, in := s.ActiveSkipSet()["ch1"]; !in {
		t.Error("ch1 not put in cooldown after no-progress cycle")
	}
}

func TestStallTracker_StallClearedOnLaterProgress(t *testing.T) {
	s := NewStallTracker(nil)
	pick := &PoolEntry{ChannelID: "ch1", Campaigns: []CampaignRef{{ID: "c1"}}}
	// Cycle 1: stalls
	s.SnapshotPick(pick, []twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	// Cycle 2: same pick now credits a minute
	s.SnapshotPick(pick, []twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 6, 60, false)})
	if _, in := s.ActiveSkipSet()["ch1"]; in {
		t.Error("stall cooldown not cleared after credit recovery")
	}
}

func TestStallTracker_ManualSurvivesProgressRecovery(t *testing.T) {
	s := NewStallTracker(nil)
	// Set a manual cooldown directly (game change scenario).
	s.SetManual("ch1", 30*time.Minute)
	// Now snapshot + credit recovery — manual must survive.
	pick := &PoolEntry{ChannelID: "ch1", Campaigns: []CampaignRef{{ID: "c1"}}}
	s.SnapshotPick(pick, []twitch.DropCampaign{makeCampaign("c1", "d1", 5, 60, false)})
	s.Apply([]twitch.DropCampaign{makeCampaign("c1", "d1", 6, 60, false)})
	if _, in := s.ActiveSkipSet()["ch1"]; !in {
		t.Error("manual cooldown was cleared by Apply credit recovery — must persist until timeout")
	}
}

func TestStallTracker_ActiveSkipSet_PrunesExpired(t *testing.T) {
	s := NewStallTracker(nil)
	// Force-insert an already-expired manual cooldown.
	s.SetManual("ch1", -time.Minute) // negative duration → already expired
	if _, in := s.ActiveSkipSet()["ch1"]; in {
		t.Error("expired entry was returned by ActiveSkipSet")
	}
	// Verify it was actually pruned (not just filtered): SetManual then
	// ActiveSkipSet should now show it newly inserted.
	s.SetManual("ch2", 10*time.Minute)
	got := s.ActiveSkipSet()
	if _, in := got["ch1"]; in {
		t.Error("ch1 still present after pruning")
	}
	if _, in := got["ch2"]; !in {
		t.Error("ch2 missing after Set/Skip roundtrip")
	}
}

func TestStallTracker_LastPickCampaignID(t *testing.T) {
	s := NewStallTracker(nil)
	if got := s.LastPickCampaignID(); got != "" {
		t.Errorf("empty tracker LastPickCampaignID = %q, want \"\"", got)
	}
	s.SnapshotPick(
		&PoolEntry{ChannelID: "ch1", Campaigns: []CampaignRef{{ID: "camp-42"}}},
		[]twitch.DropCampaign{makeCampaign("camp-42", "d1", 0, 60, false)},
	)
	if got := s.LastPickCampaignID(); got != "camp-42" {
		t.Errorf("LastPickCampaignID = %q, want camp-42", got)
	}
	s.SnapshotPick(nil, nil) // clear
	if got := s.LastPickCampaignID(); got != "" {
		t.Errorf("after clear LastPickCampaignID = %q, want \"\"", got)
	}
}
