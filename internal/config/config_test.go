package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetAndGetPinnedCampaign(t *testing.T) {
	c := &Config{}

	if c.GetPinnedCampaign() != "" {
		t.Fatalf("fresh config should have empty pin, got %q", c.GetPinnedCampaign())
	}

	c.SetPinnedCampaign("camp-abc")
	if got := c.GetPinnedCampaign(); got != "camp-abc" {
		t.Fatalf("after SetPinnedCampaign(%q), got %q", "camp-abc", got)
	}
	if !c.IsCampaignPinned("camp-abc") {
		t.Fatalf("IsCampaignPinned(%q) should be true", "camp-abc")
	}
	if c.IsCampaignPinned("other") {
		t.Fatalf("IsCampaignPinned(%q) should be false", "other")
	}

	c.SetPinnedCampaign("camp-xyz")
	if got := c.GetPinnedCampaign(); got != "camp-xyz" {
		t.Fatalf("pin overwrite failed, got %q", got)
	}
	if c.IsCampaignPinned("camp-abc") {
		t.Fatal("old pin should be cleared after overwrite")
	}

	c.SetPinnedCampaign("")
	if got := c.GetPinnedCampaign(); got != "" {
		t.Fatalf("clear pin failed, got %q", got)
	}
}

func TestPinnedCampaignSurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := &Config{
		AuthToken:        "test-token",
		PinnedCampaignID: "camp-survives",
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := loaded.GetPinnedCampaign(); got != "camp-survives" {
		t.Fatalf("pin not persisted across Save/Load, got %q", got)
	}
}

func TestPinnedCampaignBackwardCompatibleConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Old-format config with no pinned_campaign_id field
	oldJSON := `{"auth_token":"x","drops_enabled":true}`
	if err := os.WriteFile(path, []byte(oldJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := loaded.GetPinnedCampaign(); got != "" {
		t.Fatalf("missing pin field should default to empty, got %q", got)
	}
}

func TestGamesToWatchAddRemoveCaseInsensitive(t *testing.T) {
	c := &Config{}
	c.AddGameToWatch("Arena Breakout: Infinite")
	c.AddGameToWatch("Marvel Rivals")
	c.AddGameToWatch("arena breakout: infinite") // dup, different case
	if got := c.GetGamesToWatch(); len(got) != 2 {
		t.Fatalf("dup add should be no-op, got %d entries: %v", len(got), got)
	}

	c.RemoveGameFromWatch("MARVEL rivals")
	if got := c.GetGamesToWatch(); len(got) != 1 || got[0] != "Arena Breakout: Infinite" {
		t.Fatalf("case-insensitive remove failed, got %v", got)
	}

	c.RemoveGameFromWatch("not present")
	if got := c.GetGamesToWatch(); len(got) != 1 {
		t.Fatalf("removing absent game should be no-op, got %d entries", len(got))
	}
}

func TestMoveGameToWatchUpDownAndBoundaries(t *testing.T) {
	c := &Config{GamesToWatch: []string{"a", "b", "c"}}

	c.MoveGameToWatch("c", -1)
	if got := c.GamesToWatch; got[0] != "a" || got[1] != "c" || got[2] != "b" {
		t.Fatalf("after move c up, want [a c b], got %v", got)
	}

	c.MoveGameToWatch("c", -1)
	if got := c.GamesToWatch; got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("after move c up again, want [c a b], got %v", got)
	}

	c.MoveGameToWatch("c", -1) // already at top
	if got := c.GamesToWatch; got[0] != "c" {
		t.Fatalf("move at top boundary should be no-op, got %v", got)
	}

	c.MoveGameToWatch("c", +1)
	if got := c.GamesToWatch; got[0] != "a" || got[1] != "c" {
		t.Fatalf("after move c down, want [a c b], got %v", got)
	}

	c.MoveGameToWatch("missing", -1) // absent game = no-op
	if got := c.GamesToWatch; len(got) != 3 {
		t.Fatalf("move absent game should be no-op, got %v", got)
	}
}

func TestSetGamesToWatchDedupAndTrim(t *testing.T) {
	c := &Config{}
	c.SetGamesToWatch([]string{"  Arena  ", "marvel", "Arena", "", "  ", "marvel"})
	got := c.GetGamesToWatch()
	if len(got) != 2 || got[0] != "Arena" || got[1] != "marvel" {
		t.Fatalf("SetGamesToWatch should dedup case-insensitive and trim, got %v", got)
	}
}

func TestUnmarkCampaignCompleted(t *testing.T) {
	c := &Config{CompletedCampaigns: []string{"a", "b", "c"}}
	c.UnmarkCampaignCompleted("b")
	if len(c.CompletedCampaigns) != 2 || c.CompletedCampaigns[0] != "a" || c.CompletedCampaigns[1] != "c" {
		t.Fatalf("unmark should remove only the matched ID, got %v", c.CompletedCampaigns)
	}
	c.UnmarkCampaignCompleted("not-present")
	if len(c.CompletedCampaigns) != 2 {
		t.Fatalf("unmark of absent ID should be no-op, got %v", c.CompletedCampaigns)
	}
}

// TestConcurrent_NoRaces hammers the public API from many goroutines
// at once. Run with `go test -race` to catch lock omissions or
// races against the slice-getter copies. With the mu RWMutex in
// place, all accesses serialize correctly; without it the race
// detector (or just plain corruption on iteration vs append) would
// fail.
func TestConcurrent_NoRaces(t *testing.T) {
	c := &Config{}
	c.AddChannel("alpha")
	c.AddChannel("beta")
	c.AddChannel("gamma")

	const workers = 20
	const iterations = 200

	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < iterations; i++ {
				switch i % 8 {
				case 0:
					c.AddChannel("ch-tmp")
				case 1:
					c.RemoveChannel("ch-tmp")
				case 2:
					c.SetPriority("alpha", 1+(i%2))
				case 3:
					_ = c.GetChannelEntries() // copy-on-read
				case 4:
					c.AddGameToWatch("Game A")
					c.AddGameToWatch("Game B")
					c.RemoveGameFromWatch("Game A")
				case 5:
					_ = c.GetGamesToWatch()
				case 6:
					c.SetCampaignEnabled("cmp-1", i%2 == 0)
				case 7:
					_ = c.IsCampaignDisabled("cmp-1")
				}
			}
		}(w)
	}
	for i := 0; i < workers; i++ {
		<-done
	}

	// Smoke check that no internal slice was corrupted (length sane).
	if len(c.GetChannelEntries()) < 3 {
		t.Fatalf("expected at least 3 channels (alpha/beta/gamma), got %d", len(c.GetChannelEntries()))
	}
}
