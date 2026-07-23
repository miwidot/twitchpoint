package drops

import (
	"testing"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// assertConsistent fails if Percent / EtaMinutes don't match what
// Progress and Required imply. This is the invariant the display bug
// violated ("248/300min (88%)" — percentage out of sync with minutes).
func assertConsistent(t *testing.T, d ActiveDrop) {
	t.Helper()
	wantPct := 0
	wantEta := 0
	if d.Required > 0 {
		wantPct = (d.Progress * 100) / d.Required
		if wantPct > 100 {
			wantPct = 100
		}
		wantEta = d.Required - d.Progress
		if wantEta < 0 {
			wantEta = 0
		}
	}
	if d.Percent != wantPct {
		t.Fatalf("Percent %d inconsistent with %d/%d (want %d)", d.Percent, d.Progress, d.Required, wantPct)
	}
	if d.EtaMinutes != wantEta {
		t.Fatalf("EtaMinutes %d inconsistent with %d/%d (want %d)", d.EtaMinutes, d.Progress, d.Required, wantEta)
	}
}

func TestRecomputeDerived(t *testing.T) {
	cases := []struct {
		progress, required, wantPct, wantEta int
	}{
		{248, 300, 82, 52},
		{300, 300, 100, 0},
		{330, 300, 100, 0}, // over 100% clamps, eta floors at 0
		{0, 300, 0, 300},
		{50, 0, 0, 0}, // no required → both zeroed, never a stale percentage
	}
	for _, c := range cases {
		d := ActiveDrop{Progress: c.progress, Required: c.required}
		d.recomputeDerived()
		if d.Percent != c.wantPct || d.EtaMinutes != c.wantEta {
			t.Fatalf("progress=%d required=%d → pct=%d eta=%d, want pct=%d eta=%d",
				c.progress, c.required, d.Percent, d.EtaMinutes, c.wantPct, c.wantEta)
		}
	}
}

// TestApplyProgressUpdate_KeepsPercentConsistent is the display-bug
// regression: a progress event that advances Progress must recompute
// Percent/EtaMinutes so they can never be left stale. Seed the row with a
// deliberately wrong (stale) Percent to prove the update overwrites it.
func TestApplyProgressUpdate_KeepsPercentConsistent(t *testing.T) {
	s := &Service{
		cfg:           &config.Config{},
		log:           func(string, ...interface{}) {},
		writeLogFile:  func(string) {},
		campaignCache: map[string]twitch.DropCampaign{},
		activeDrops: []ActiveDrop{{
			CampaignID: "camp-1",
			Progress:   248,
			Required:   300,
			Percent:    88, // stale/wrong on purpose (248/300 = 82%)
			EtaMinutes: 120, // stale/wrong on purpose (300-248 = 52)
		}},
	}

	s.ApplyProgressUpdate(twitch.DropProgressData{
		CampaignID:            "camp-1",
		DropID:                "drop-1",
		CurrentMinutesWatched: 250,
		// RequiredMinutesWatched intentionally 0: the payload often omits
		// it, which is exactly when the old code skipped the Percent/Eta
		// recompute and left the stale values on screen.
	})

	assertConsistent(t, s.activeDrops[0])
	if s.activeDrops[0].Progress != 250 {
		t.Fatalf("Progress not advanced: got %d", s.activeDrops[0].Progress)
	}
	if s.activeDrops[0].Percent != 83 { // 250/300
		t.Fatalf("Percent not refreshed from advanced progress: got %d", s.activeDrops[0].Percent)
	}
}

// TestApplyProgressUpdate_RequiredResolvedFromPayload: when the payload
// carries a required value, it's adopted and the derived fields follow it.
func TestApplyProgressUpdate_RequiredResolvedFromPayload(t *testing.T) {
	s := &Service{
		cfg:           &config.Config{},
		log:           func(string, ...interface{}) {},
		writeLogFile:  func(string) {},
		campaignCache: map[string]twitch.DropCampaign{},
		activeDrops: []ActiveDrop{{
			CampaignID: "camp-1",
			Progress:   10,
			Required:   60,
			Percent:    16,
			EtaMinutes: 50,
		}},
	}

	s.ApplyProgressUpdate(twitch.DropProgressData{
		CampaignID:             "camp-1",
		DropID:                 "drop-2",
		CurrentMinutesWatched:  30,
		RequiredMinutesWatched: 120,
	})

	assertConsistent(t, s.activeDrops[0])
	if s.activeDrops[0].Required != 120 || s.activeDrops[0].Percent != 25 {
		t.Fatalf("expected Required=120 Percent=25, got Required=%d Percent=%d",
			s.activeDrops[0].Required, s.activeDrops[0].Percent)
	}
}
