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
