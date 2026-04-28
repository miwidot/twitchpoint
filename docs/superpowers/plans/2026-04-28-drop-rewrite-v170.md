# v1.7.0 — Drop Subsystem Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the parallel multi-drop architecture (drops.go + farmer.go failover/stall machinery, ~1500 LoC of stateful coordination) with a single channel-pool selector (~250 LoC) inspired by TwitchDropsMiner.

**Architecture:** A single `DropSelector` builds a pool of live drops-enabled channels each cycle, sorts by `(pinned, remaining_time, viewer_count)`, picks one. The picked channel gets `HasActiveDrop=true`, which makes the existing `rotateChannels` give it Spade slot 1 automatically. No failover, no stall detection, no transferDrop — every cycle is a fresh selection.

**Tech Stack:** Go 1.22+, no new dependencies. Tests use Go's built-in `testing` package (table-driven). Existing `internal/twitch/*` GQL helpers stay unchanged.

**Spec:** `docs/superpowers/specs/2026-04-28-drop-rewrite-v170-design.md`

**Branch:** main (project allows direct main commits; user CLAUDE.md: "Work directly on main for solo development").

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/config/config.go` | Modify | Add `PinnedCampaignID` field + helpers |
| `internal/config/config_test.go` | Create | Tests for pin helpers + persistence |
| `internal/farmer/dropselector.go` | Create | Pure channel-pool selector logic |
| `internal/farmer/dropselector_test.go` | Create | Table-driven tests for selector |
| `internal/farmer/drops.go` | Rewrite | Slim coordinator: poll → select → assign Spade |
| `internal/farmer/farmer.go` | Modify | Strip `handleDropFailover`, `transferDrop`, `verifyTempChannelHealth`, `findLiveFromGameDirectory*` |
| `internal/web/server.go` | Modify | Add `PUT /api/drops/{id}/pin`, augment `/api/drops` response |
| `internal/web/static/index.html` | Modify | Pin button column, Status/QueueIndex display, HTML-escape helper |
| `cmd/twitchpoint/main.go` | Modify | Version bump 1.6.5 → 1.7.0 |
| `changelog.txt` | Modify | Add v1.7.0 entry at top |

**Decomposition principle:** The selector is a pure function of (campaigns, config, stream-source). It returns a pick + queue without any side effects on `Farmer.channels` or Spade. The drops.go coordinator applies the result. This separation makes the selector trivially testable and the coordinator very small.

---

## Task 1: Add `PinnedCampaignID` to Config

**Files:**
- Modify: `internal/config/config.go` (struct definition around line 1-50, add helpers at end)
- Create: `internal/config/config_test.go`

- [ ] **Step 1.1: Read current Config struct to confirm field placement**

Run: `grep -n "type Config struct" internal/config/config.go`

Expected output: line number of struct definition. Note the position of `CompletedCampaigns` field — `PinnedCampaignID` will go right after it.

- [ ] **Step 1.2: Add `PinnedCampaignID` field to Config struct**

In `internal/config/config.go`, find the line:
```go
CompletedCampaigns    []string `json:"completed_campaigns,omitempty"`
```
Add immediately after it:
```go
PinnedCampaignID      string   `json:"pinned_campaign_id,omitempty"`         // single campaign to prioritize over remaining-time sort (empty = no pin)
```

- [ ] **Step 1.3: Add pin helper methods**

At the end of `internal/config/config.go` (after the existing `IsCampaignCompleted` method), append:

```go
// SetPinnedCampaign atomically sets the pinned campaign. Pass empty string to clear.
// Only one campaign can be pinned at a time — calling this overwrites any previous pin.
func (c *Config) SetPinnedCampaign(campaignID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PinnedCampaignID = campaignID
}

// IsCampaignPinned returns true if the given campaign is the currently pinned one.
func (c *Config) IsCampaignPinned(campaignID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PinnedCampaignID != "" && c.PinnedCampaignID == campaignID
}

// GetPinnedCampaign returns the currently pinned campaign ID, or empty string if none.
func (c *Config) GetPinnedCampaign() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PinnedCampaignID
}
```

> Note: this assumes Config has a `mu sync.RWMutex` field. If grep below shows otherwise, drop the lock calls.

- [ ] **Step 1.4: Verify Config has the expected mutex**

Run: `grep -n "mu " internal/config/config.go | head -3`

If no `mu sync.RWMutex` exists in Config, remove all `c.mu.Lock/RLock/Unlock/RUnlock` lines from Step 1.3 helpers (the helpers will then be non-thread-safe but match existing pattern). If mutex exists, leave the locks.

- [ ] **Step 1.5: Write failing tests**

Create `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 1.6: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`

Expected output: `PASS` for all three tests (`TestSetAndGetPinnedCampaign`, `TestPinnedCampaignSurvivesSaveLoad`, `TestPinnedCampaignBackwardCompatibleConfig`).

If `Load` doesn't exist or has a different signature, adapt the test to use whatever the project's actual loader is named (check via `grep "^func Load" internal/config/config.go`).

- [ ] **Step 1.7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
config: add PinnedCampaignID field and pin helpers

Single campaign pin used by v1.7.0 selector to override default
remaining-time sort. Backward compatible — old configs load with empty pin.
EOF
)"
```

---

## Task 2: Create selector types

**Files:**
- Create: `internal/farmer/dropselector.go`

- [ ] **Step 2.1: Create new file with package declaration and types**

Create `internal/farmer/dropselector.go`:

```go
package farmer

import (
	"sort"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// streamSource is the minimal GQL interface the selector needs. Mocked in tests.
type streamSource interface {
	GetGameStreamsDropsEnabled(gameName string, limit int) ([]twitch.GameStream, error)
}

// CampaignRef is a lightweight reference to a campaign that a pool entry serves.
type CampaignRef struct {
	ID            string
	Name          string
	GameName      string
	EndAt         time.Time
	RemainingTime time.Duration
	IsPinned      bool
}

// PoolEntry represents one candidate channel in the selector's pool.
// A channel may serve multiple campaigns simultaneously (Twitch credits
// whichever can_earn while we watch).
type PoolEntry struct {
	ChannelID    string        // Twitch broadcaster user ID
	ChannelLogin string        // lowercase login
	DisplayName  string
	BroadcastID  string
	ViewerCount  int
	Campaigns    []CampaignRef // 1+ eligible campaigns this channel serves; sorted with highest priority first
}

// DropSelector is a pure pick-a-channel function with no side effects on Farmer state.
// Construction takes everything it needs as dependencies so tests can substitute mocks.
type DropSelector struct {
	cfg     *config.Config
	streams streamSource
	now     func() time.Time // injectable for deterministic tests
}

// NewDropSelector constructs a selector with the production stream source.
func NewDropSelector(cfg *config.Config, gql *twitch.GQLClient) *DropSelector {
	return &DropSelector{
		cfg:     cfg,
		streams: gql,
		now:     time.Now,
	}
}
```

- [ ] **Step 2.2: Verify the file compiles**

Run: `go build ./internal/farmer/`

Expected: exit 0, no output. If `twitch.GameStream` is named differently, run `grep "type GameStream" internal/twitch/*.go` and adapt the import/type.

- [ ] **Step 2.3: Commit**

```bash
git add internal/farmer/dropselector.go
git commit -m "farmer: scaffold DropSelector with pool entry types"
```

---

## Task 3: Selector — `filterEligibleCampaigns`

**Files:**
- Modify: `internal/farmer/dropselector.go` (add function)
- Create: `internal/farmer/dropselector_test.go`

- [ ] **Step 3.1: Add filterEligibleCampaigns to dropselector.go**

Append to `internal/farmer/dropselector.go`:

```go
// filterEligibleCampaigns drops campaigns that are not currently farmable:
// non-active status, expired, account not connected, disabled by user,
// already completed, or have no watchable (non-sub-only, non-claimed) drops.
func (s *DropSelector) filterEligibleCampaigns(campaigns []twitch.DropCampaign) []twitch.DropCampaign {
	now := s.now()
	out := make([]twitch.DropCampaign, 0, len(campaigns))

	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(now) {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if s.cfg.IsCampaignDisabled(c.ID) {
			continue
		}
		if s.cfg.IsCampaignCompleted(c.ID) {
			continue
		}
		// Need at least one watchable, unclaimed drop
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasWatchable = true
				break
			}
		}
		if !hasWatchable {
			continue
		}
		out = append(out, c)
	}

	return out
}
```

- [ ] **Step 3.2: Write the failing test**

Create `internal/farmer/dropselector_test.go`:

```go
package farmer

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// fixed reference time for deterministic tests
var testNow = time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)

func newTestSelector(cfg *config.Config) *DropSelector {
	return &DropSelector{
		cfg:     cfg,
		streams: nil,
		now:     func() time.Time { return testNow },
	}
}

func makeWatchableDrop() twitch.TimeBasedDrop {
	return twitch.TimeBasedDrop{
		ID:                     "drop-1",
		Name:                   "Test Drop",
		RequiredMinutesWatched: 60,
		IsClaimed:              false,
	}
}

func TestFilterEligibleCampaigns(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisabledCampaigns = []string{"camp-disabled"}
	cfg.CompletedCampaigns = []string{"camp-completed"}

	tests := []struct {
		name     string
		campaign twitch.DropCampaign
		want     bool // true = passes filter
	}{
		{
			name: "active connected with watchable drop passes",
			campaign: twitch.DropCampaign{
				ID: "camp-good", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: true,
		},
		{
			name: "expired campaign dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-expired", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(-1 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "non-ACTIVE status dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-expired-status", Status: "EXPIRED", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "account not connected dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-noacct", Status: "ACTIVE", IsAccountConnected: false,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "disabled by user dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-disabled", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "marked completed dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-completed", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "sub-only drops dropped (RequiredMinutesWatched=0)",
			campaign: twitch.DropCampaign{
				ID: "camp-subonly", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{
					{ID: "d1", RequiredMinutesWatched: 0, IsClaimed: false},
				},
			},
			want: false,
		},
		{
			name: "all drops claimed dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-allclaimed", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{
					{ID: "d1", RequiredMinutesWatched: 60, IsClaimed: true},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := newTestSelector(cfg)
			out := sel.filterEligibleCampaigns([]twitch.DropCampaign{tt.campaign})
			got := len(out) == 1
			if got != tt.want {
				t.Fatalf("got %d eligible (%v), want %v", len(out), got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 3.3: Run tests, verify pass**

Run: `go test ./internal/farmer/ -run TestFilterEligibleCampaigns -v`

Expected: `PASS` for all 8 sub-tests.

If `IsCampaignDisabled` / `IsCampaignCompleted` don't exist on Config, run `grep "func.*Config.*Campaign" internal/config/config.go` and adapt.

- [ ] **Step 3.4: Commit**

```bash
git add internal/farmer/dropselector.go internal/farmer/dropselector_test.go
git commit -m "farmer: DropSelector eligibility filter with table-driven tests"
```

---

## Task 4: Selector — `buildPool`

**Files:**
- Modify: `internal/farmer/dropselector.go` (add function)
- Modify: `internal/farmer/dropselector_test.go` (add tests + fake stream source)

- [ ] **Step 4.1: Add buildPool to dropselector.go**

Append to `internal/farmer/dropselector.go`:

```go
// buildPool turns an eligible-campaign list into a deduped pool of candidate
// channels. For campaigns with an allow list, it queries the drops-enabled
// game directory and intersects with that list. For unrestricted campaigns,
// the top drops-enabled streams for the game become candidates directly.
//
// Channels appearing in multiple campaigns are deduped — a single PoolEntry
// carries all the campaigns it serves.
func (s *DropSelector) buildPool(eligible []twitch.DropCampaign) []*PoolEntry {
	pinnedID := s.cfg.GetPinnedCampaign()

	// Game-name → cached directory result, so we hit GQL at most once per game per cycle.
	dirCache := make(map[string][]twitch.GameStream)
	getDir := func(gameName string) []twitch.GameStream {
		if cached, ok := dirCache[gameName]; ok {
			return cached
		}
		streams, err := s.streams.GetGameStreamsDropsEnabled(gameName, 100)
		if err != nil {
			dirCache[gameName] = nil // negative cache for the cycle
			return nil
		}
		dirCache[gameName] = streams
		return streams
	}

	byChannel := make(map[string]*PoolEntry) // channelID → entry

	for _, c := range eligible {
		ref := CampaignRef{
			ID:            c.ID,
			Name:          c.Name,
			GameName:      c.GameName,
			EndAt:         c.EndAt,
			RemainingTime: time.Until(c.EndAt),
			IsPinned:      c.ID == pinnedID,
		}
		if c.GameName == "" {
			continue // can't query directory without game name
		}

		streams := getDir(c.GameName)
		if len(streams) == 0 {
			continue
		}

		// Build allowed-channel lookup if campaign has restrictions
		var allowedByID map[string]bool
		var allowedByLogin map[string]bool
		hasAllow := len(c.Channels) > 0
		if hasAllow {
			allowedByID = make(map[string]bool, len(c.Channels))
			allowedByLogin = make(map[string]bool, len(c.Channels))
			for _, ch := range c.Channels {
				if ch.ID != "" {
					allowedByID[ch.ID] = true
				}
				if ch.Name != "" {
					allowedByLogin[strings.ToLower(ch.Name)] = true
				}
				if ch.DisplayName != "" {
					allowedByLogin[strings.ToLower(ch.DisplayName)] = true
				}
			}
		}

		for _, st := range streams {
			login := strings.ToLower(st.BroadcasterLogin)

			if hasAllow {
				// Skip if not in allow list
				if !allowedByID[st.BroadcasterID] && !allowedByLogin[login] {
					continue
				}
			}

			entry, exists := byChannel[st.BroadcasterID]
			if !exists {
				entry = &PoolEntry{
					ChannelID:    st.BroadcasterID,
					ChannelLogin: login,
					DisplayName:  st.BroadcasterDisplayName,
					BroadcastID:  st.StreamID,
					ViewerCount:  st.ViewerCount,
				}
				byChannel[st.BroadcasterID] = entry
			}
			entry.Campaigns = append(entry.Campaigns, ref)
		}
	}

	// Convert to slice
	pool := make([]*PoolEntry, 0, len(byChannel))
	for _, e := range byChannel {
		pool = append(pool, e)
	}
	return pool
}
```

- [ ] **Step 4.2: Verify required twitch.GameStream fields exist**

Run: `grep -A 8 "type GameStream struct" internal/twitch/gql.go`

Expected fields: `BroadcasterID`, `BroadcasterLogin`, `BroadcasterDisplayName`, `StreamID`, `ViewerCount`. If field names differ (e.g. `Login` instead of `BroadcasterLogin`), adapt the code in Step 4.1 to match the real struct.

- [ ] **Step 4.3: Add fake stream source to test file**

In `internal/farmer/dropselector_test.go`, add at the bottom:

```go
// fakeStreamSource is a deterministic in-memory stream source for tests.
type fakeStreamSource struct {
	byGame map[string][]twitch.GameStream
	calls  map[string]int // game name → how often queried
}

func (f *fakeStreamSource) GetGameStreamsDropsEnabled(gameName string, limit int) ([]twitch.GameStream, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[gameName]++
	streams := f.byGame[gameName]
	if len(streams) > limit {
		return streams[:limit], nil
	}
	return streams, nil
}

func newSelectorWithStreams(cfg *config.Config, src *fakeStreamSource) *DropSelector {
	return &DropSelector{
		cfg:     cfg,
		streams: src,
		now:     func() time.Time { return testNow },
	}
}
```

- [ ] **Step 4.4: Add buildPool tests**

Append to `internal/farmer/dropselector_test.go`:

```go
func TestBuildPool_AllowListIntersection(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Arena Breakout: Infinite": {
			{BroadcasterID: "1", BroadcasterLogin: "buggy", BroadcasterDisplayName: "Buggy", StreamID: "s1", ViewerCount: 700},
			{BroadcasterID: "2", BroadcasterLogin: "kritikal", BroadcasterDisplayName: "kritikal", StreamID: "s2", ViewerCount: 200},
			{BroadcasterID: "3", BroadcasterLogin: "randomdude", BroadcasterDisplayName: "RandomDude", StreamID: "s3", ViewerCount: 100},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "abi-partner", Name: "ABI Partner-Only Drops",
		Status: "ACTIVE", IsAccountConnected: true, GameName: "Arena Breakout: Infinite",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{
			{ID: "1", Name: "buggy"},
			{ID: "2", Name: "kritikal"},
			{ID: "999", Name: "offline_streamer"},
		},
	}

	pool := sel.buildPool([]twitch.DropCampaign{camp})
	if len(pool) != 2 {
		t.Fatalf("want 2 pool entries (buggy + kritikal), got %d", len(pool))
	}
	logins := map[string]bool{}
	for _, e := range pool {
		logins[e.ChannelLogin] = true
	}
	if !logins["buggy"] || !logins["kritikal"] {
		t.Fatalf("expected buggy + kritikal, got %v", logins)
	}
	if logins["randomdude"] {
		t.Fatal("randomdude should be filtered (not in allow list)")
	}
}

func TestBuildPool_UnrestrictedCampaign(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Marvel Rivals": {
			{BroadcasterID: "10", BroadcasterLogin: "streamer_a", StreamID: "sa", ViewerCount: 5000},
			{BroadcasterID: "11", BroadcasterLogin: "streamer_b", StreamID: "sb", ViewerCount: 3000},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "rivals-s7", Status: "ACTIVE", IsAccountConnected: true, GameName: "Marvel Rivals",
		EndAt: testNow.Add(5 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: nil, // unrestricted
	}

	pool := sel.buildPool([]twitch.DropCampaign{camp})
	if len(pool) != 2 {
		t.Fatalf("unrestricted campaign should add all directory streams, got %d", len(pool))
	}
}

func TestBuildPool_DedupesAcrossCampaigns(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {
			{BroadcasterID: "1", BroadcasterLogin: "buggy", StreamID: "s1", ViewerCount: 700},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	c1 := twitch.DropCampaign{
		ID: "abi-1", Name: "Partner-Only", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{{ID: "1", Name: "buggy"}},
	}
	c2 := twitch.DropCampaign{
		ID: "abi-2", Name: "Support Partners", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(20 * 24 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{{ID: "1", Name: "buggy"}},
	}

	pool := sel.buildPool([]twitch.DropCampaign{c1, c2})
	if len(pool) != 1 {
		t.Fatalf("buggy in 2 campaigns should yield 1 deduped pool entry, got %d", len(pool))
	}
	if len(pool[0].Campaigns) != 2 {
		t.Fatalf("deduped entry should carry both campaigns, got %d", len(pool[0].Campaigns))
	}
}

func TestBuildPool_DirectoryQueriedOncePerGame(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {{BroadcasterID: "1", BroadcasterLogin: "buggy", StreamID: "s1"}},
	}}
	sel := newSelectorWithStreams(cfg, src)

	c1 := twitch.DropCampaign{
		ID: "abi-1", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}
	c2 := twitch.DropCampaign{
		ID: "abi-2", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(5 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	sel.buildPool([]twitch.DropCampaign{c1, c2})
	if got := src.calls["ABI"]; got != 1 {
		t.Fatalf("directory should be queried once per cycle per game, got %d calls", got)
	}
}
```

- [ ] **Step 4.5: Run tests, verify pass**

Run: `go test ./internal/farmer/ -run TestBuildPool -v`

Expected: `PASS` for all four tests.

If a field name in `twitch.GameStream` or `twitch.DropChannel` differs from what the tests use, fix the test data to match. Run `grep "type DropChannel\|type GameStream" internal/twitch/*.go` to verify.

- [ ] **Step 4.6: Commit**

```bash
git add internal/farmer/dropselector.go internal/farmer/dropselector_test.go
git commit -m "farmer: DropSelector buildPool with allow-list intersection and dedup"
```

---

## Task 5: Selector — sort + pick + `Select()` orchestration

**Files:**
- Modify: `internal/farmer/dropselector.go`
- Modify: `internal/farmer/dropselector_test.go`

- [ ] **Step 5.1: Add sortPool and Select to dropselector.go**

Append to `internal/farmer/dropselector.go`:

```go
// sortPool sorts entries in priority order:
//   1. Channels serving any pinned campaign first
//   2. Then by earliest endAt across the channel's campaigns (closest expiry wins)
//   3. Then by viewer count desc (stability tie-break, mild preference for popular streams)
func (s *DropSelector) sortPool(pool []*PoolEntry) {
	// Pre-compute earliest end and pin status per entry for stable comparison
	type cached struct {
		hasPinned bool
		minEnd    time.Time
	}
	keys := make(map[*PoolEntry]cached, len(pool))
	for _, e := range pool {
		var c cached
		first := true
		for _, ref := range e.Campaigns {
			if ref.IsPinned {
				c.hasPinned = true
			}
			if first || ref.EndAt.Before(c.minEnd) {
				c.minEnd = ref.EndAt
				first = false
			}
		}
		keys[e] = c
	}

	sort.SliceStable(pool, func(i, j int) bool {
		ki, kj := keys[pool[i]], keys[pool[j]]
		// Pinned first
		if ki.hasPinned != kj.hasPinned {
			return ki.hasPinned
		}
		// Earlier endAt first
		if !ki.minEnd.Equal(kj.minEnd) {
			return ki.minEnd.Before(kj.minEnd)
		}
		// Higher viewers first
		return pool[i].ViewerCount > pool[j].ViewerCount
	})

	// Also reorder each entry's own Campaigns list by (pinned, endAt) so
	// callers can index Campaigns[0] for "primary campaign of this channel".
	for _, e := range pool {
		sort.SliceStable(e.Campaigns, func(i, j int) bool {
			a, b := e.Campaigns[i], e.Campaigns[j]
			if a.IsPinned != b.IsPinned {
				return a.IsPinned
			}
			return a.EndAt.Before(b.EndAt)
		})
	}
}

// Select runs the full pipeline: filter → buildPool → sort → pick.
// Returns (pickedChannel, sortedPool). pickedChannel is nil if pool empty.
// The returned pool is sorted; callers can use pool[1:] as the queue for UI.
func (s *DropSelector) Select(campaigns []twitch.DropCampaign) (*PoolEntry, []*PoolEntry) {
	eligible := s.filterEligibleCampaigns(campaigns)
	if len(eligible) == 0 {
		return nil, nil
	}
	pool := s.buildPool(eligible)
	if len(pool) == 0 {
		return nil, nil
	}
	s.sortPool(pool)
	return pool[0], pool
}
```

- [ ] **Step 5.2: Add sort and Select tests**

Append to `internal/farmer/dropselector_test.go`:

```go
func TestSortPool_PinnedFirst(t *testing.T) {
	cfg := &config.Config{}
	cfg.PinnedCampaignID = "pinned-camp"
	sel := newTestSelector(cfg)

	a := &PoolEntry{ChannelLogin: "a", Campaigns: []CampaignRef{
		{ID: "other", EndAt: testNow.Add(1 * time.Hour), IsPinned: false},
	}}
	b := &PoolEntry{ChannelLogin: "b", Campaigns: []CampaignRef{
		{ID: "pinned-camp", EndAt: testNow.Add(10 * time.Hour), IsPinned: true},
	}}

	pool := []*PoolEntry{a, b}
	sel.sortPool(pool)
	if pool[0].ChannelLogin != "b" {
		t.Fatalf("pinned channel should sort first, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_EarlierEndAtFirst(t *testing.T) {
	cfg := &config.Config{}
	sel := newTestSelector(cfg)

	near := &PoolEntry{ChannelLogin: "near", Campaigns: []CampaignRef{
		{EndAt: testNow.Add(2 * time.Hour)},
	}}
	far := &PoolEntry{ChannelLogin: "far", Campaigns: []CampaignRef{
		{EndAt: testNow.Add(20 * time.Hour)},
	}}

	pool := []*PoolEntry{far, near}
	sel.sortPool(pool)
	if pool[0].ChannelLogin != "near" {
		t.Fatalf("near-expiry channel should sort first, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_ViewerCountTieBreak(t *testing.T) {
	cfg := &config.Config{}
	sel := newTestSelector(cfg)

	endAt := testNow.Add(2 * time.Hour)
	low := &PoolEntry{ChannelLogin: "low", ViewerCount: 100, Campaigns: []CampaignRef{{EndAt: endAt}}}
	high := &PoolEntry{ChannelLogin: "high", ViewerCount: 1000, Campaigns: []CampaignRef{{EndAt: endAt}}}

	pool := []*PoolEntry{low, high}
	sel.sortPool(pool)
	if pool[0].ChannelLogin != "high" {
		t.Fatalf("higher viewer count should win tie, got %s", pool[0].ChannelLogin)
	}
}

func TestSelect_EmptyPoolReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {}, // game returns no live streams
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "abi", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	pick, queue := sel.Select([]twitch.DropCampaign{camp})
	if pick != nil || queue != nil {
		t.Fatalf("empty stream directory should yield nil pick, got %v / %v", pick, queue)
	}
}

func TestSelect_PinForcesNonClosestExpiry(t *testing.T) {
	cfg := &config.Config{}
	cfg.PinnedCampaignID = "pinned-far"
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"GameA": {{BroadcasterID: "1", BroadcasterLogin: "near_streamer", StreamID: "sn"}},
		"GameB": {{BroadcasterID: "2", BroadcasterLogin: "far_streamer", StreamID: "sf"}},
	}}
	sel := newSelectorWithStreams(cfg, src)

	near := twitch.DropCampaign{
		ID: "near", Status: "ACTIVE", IsAccountConnected: true, GameName: "GameA",
		EndAt: testNow.Add(2 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}
	pinnedFar := twitch.DropCampaign{
		ID: "pinned-far", Status: "ACTIVE", IsAccountConnected: true, GameName: "GameB",
		EndAt: testNow.Add(20 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	pick, _ := sel.Select([]twitch.DropCampaign{near, pinnedFar})
	if pick == nil || pick.ChannelLogin != "far_streamer" {
		t.Fatalf("pin should override closest-expiry sort, picked %v", pick)
	}
}
```

- [ ] **Step 5.3: Run tests, verify pass**

Run: `go test ./internal/farmer/ -run "TestSortPool|TestSelect" -v`

Expected: `PASS` for all 5 tests.

- [ ] **Step 5.4: Commit**

```bash
git add internal/farmer/dropselector.go internal/farmer/dropselector_test.go
git commit -m "farmer: DropSelector sortPool + Select orchestration"
```

---

## Task 6: Augment `ActiveDrop` API struct

**Files:**
- Modify: `internal/farmer/drops.go` (struct around line 13)

- [ ] **Step 6.1: Read current ActiveDrop struct**

Run: `sed -n '13,30p' internal/farmer/drops.go`

Note the existing fields. New fields go BEFORE the closing `}`.

- [ ] **Step 6.2: Add new fields to ActiveDrop**

In `internal/farmer/drops.go`, find the `ActiveDrop` struct (around line 13-28) and add these fields right before the closing `}`:

```go
	Status     string `json:"status"`       // ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED
	IsPinned   bool   `json:"is_pinned"`
	QueueIndex int    `json:"queue_index"`  // 1-based for ACTIVE/QUEUED/IDLE; 0 otherwise
	EtaMinutes int    `json:"eta_minutes"`  // RequiredMinutesWatched - CurrentMinutesWatched of next-to-claim drop
```

- [ ] **Step 6.3: Verify compile**

Run: `go build ./...`

Expected: exit 0. (Existing code that constructs ActiveDrop will leave new fields zero-valued — that's fine for now, will be filled in Task 7.)

- [ ] **Step 6.4: Commit**

```bash
git add internal/farmer/drops.go
git commit -m "farmer: extend ActiveDrop API struct with v1.7.0 fields"
```

---

## Task 7: Rewrite `processDrops` to use selector

**Files:**
- Modify: `internal/farmer/drops.go` (the `processDrops` function, currently ~290 lines from line 65)

This task replaces the entire body of `processDrops`. The drop-claiming logic and the inventory polling loop stay; everything between is replaced.

- [ ] **Step 7.1: Add selector field to dropState**

In `internal/farmer/drops.go`, find the `dropState` struct (around line 30-37). Replace it with:

```go
// dropState holds internal state for the drop tracker.
type dropState struct {
	mu            sync.RWMutex
	activeDrops   []ActiveDrop                          // for /api/drops
	queuedDrops   []ActiveDrop                          // for /api/drops, status=QUEUED
	idleDrops     []ActiveDrop                          // for /api/drops, status=IDLE
	campaignCache map[string]twitch.DropCampaign        // campaignID -> campaign, rebuilt each cycle
	currentPickID string                                // ChannelID currently assigned the drop slot, "" if none

	selector *DropSelector
}
```

- [ ] **Step 7.2: Initialize selector in Farmer constructor**

Run: `grep -n "drops.*=.*&dropState\|f\.drops\s*=\|f.drops = " internal/farmer/farmer.go`

Find where `f.drops` is initialized. Update that line so it includes the selector:

```go
f.drops = &dropState{
    selector: NewDropSelector(f.cfg, f.gql),
}
```

If `f.cfg` or `f.gql` aren't accessible at that point, look for how Farmer holds them (`grep "cfg\|gql" internal/farmer/farmer.go | head -10`) and use the actual field names.

- [ ] **Step 7.3: Replace processDrops body**

In `internal/farmer/drops.go`, find `func (f *Farmer) processDrops()` (around line 65). Replace the entire function (from `func` to the matching closing brace) with:

```go
// processDrops fetches the inventory, runs the selector, applies the pick to
// Spade, and auto-claims any completed drops. Called every 5 min by dropCheckLoop.
func (f *Farmer) processDrops() {
	campaigns, err := f.gql.GetDropsInventory()
	if err != nil {
		f.addLog("[Drops] Failed to fetch inventory: %v", err)
		return
	}

	f.writeLogFile(fmt.Sprintf("[Drops] Inventory returned %d campaigns", len(campaigns)))

	// 1. Auto-claim any drops that are complete and have an instance ID.
	//    Auto-mark campaigns whose watchable drops are all claimed as completed.
	f.autoClaimAndMarkCompleted(campaigns)

	// 2. Run the selector on the (now-updated) inventory.
	pick, pool := f.drops.selector.Select(campaigns)

	// 3. Build per-campaign UI rows: ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED.
	active, queued, idle := f.buildDropRows(campaigns, pick, pool)

	// 4. Rebuild campaign cache (needed by web UI for endAt lookups).
	newCache := make(map[string]twitch.DropCampaign, len(campaigns))
	for _, c := range campaigns {
		newCache[c.ID] = c
	}

	// 5. Apply pick: register channel as temp if new, set HasActiveDrop.
	//    Clear HasActiveDrop on any other temp channel that was the previous pick.
	f.applySelectorPick(pick, campaigns)

	// 6. Store rows + cache atomically.
	f.drops.mu.Lock()
	f.drops.activeDrops = active
	f.drops.queuedDrops = queued
	f.drops.idleDrops = idle
	f.drops.campaignCache = newCache
	if pick != nil {
		f.drops.currentPickID = pick.ChannelID
	} else {
		f.drops.currentPickID = ""
	}
	f.drops.mu.Unlock()

	// 7. Drop existing temp channels that are no longer the pick.
	f.cleanupNonPickedTempChannels(pick)

	// 8. Trigger rotation so Spade slot 1 reflects the new pick (HasActiveDrop=true → P0).
	f.rotateChannels()

	if pick != nil {
		campaignNames := make([]string, len(pick.Campaigns))
		for i, c := range pick.Campaigns {
			campaignNames[i] = c.Name
		}
		f.addLog("[Drops/Pool] picked %s (campaigns: %s)", pick.DisplayName, strings.Join(campaignNames, ", "))
	} else {
		f.addLog("[Drops/Pool] empty pool — drops idle, slots free for points")
	}
}
```

- [ ] **Step 7.4: Add the helper methods referenced above**

Append to `internal/farmer/drops.go`:

```go
// autoClaimAndMarkCompleted handles drop claims and marks fully-claimed
// campaigns as completed in config. Pure inventory-side bookkeeping.
func (f *Farmer) autoClaimAndMarkCompleted(campaigns []twitch.DropCampaign) {
	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if f.cfg.IsCampaignCompleted(c.ID) {
			continue
		}

		allClaimed := true
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			hasWatchable = true
			if d.IsClaimed {
				continue
			}
			allClaimed = false
			if d.IsComplete() && d.DropInstanceID != "" {
				name := d.BenefitName
				if name == "" {
					name = d.Name
				}
				instanceID := d.DropInstanceID
				dropName := name
				campaignName := c.Name
				go func() {
					if err := f.gql.ClaimDrop(instanceID); err != nil {
						f.addLog("[Drops] Failed to claim %s: %v", dropName, err)
					} else {
						f.addLog("[Drops] Claimed: %s (%s)", dropName, campaignName)
					}
				}()
			}
		}

		if hasWatchable && allClaimed {
			f.cfg.MarkCampaignCompleted(c.ID)
			f.cfg.Save()
			f.addLog("[Drops] Campaign %q fully claimed — marked as completed", c.Name)
		}
	}
}

// buildDropRows produces the per-campaign UI rows for the web API: ACTIVE for
// the pick's primary campaign, QUEUED for other eligible campaigns whose
// channels are in the pool, IDLE for eligible campaigns with no live channel
// right now, and synthetic DISABLED/COMPLETED rows for state visibility.
func (f *Farmer) buildDropRows(
	campaigns []twitch.DropCampaign,
	pick *PoolEntry,
	pool []*PoolEntry,
) (active, queued, idle []ActiveDrop) {
	pinnedID := f.cfg.GetPinnedCampaign()

	// Index pool entries by campaign ID so we know which campaigns have at least one live channel.
	campaignsInPool := make(map[string]*PoolEntry)
	for _, e := range pool {
		for _, ref := range e.Campaigns {
			if _, exists := campaignsInPool[ref.ID]; !exists {
				campaignsInPool[ref.ID] = e
			}
		}
	}

	pickedCampaignIDs := make(map[string]bool)
	if pick != nil {
		for _, ref := range pick.Campaigns {
			pickedCampaignIDs[ref.ID] = true
		}
	}

	queueIdx := 1
	for _, c := range campaigns {
		// Skip non-active and expired entirely (don't surface to UI).
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(time.Now()) {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}

		row := campaignToRow(c, pinnedID)

		switch {
		case f.cfg.IsCampaignDisabled(c.ID):
			row.Status = "DISABLED"
			active = append(active, row) // surface in main list, sorted naturally last
		case f.cfg.IsCampaignCompleted(c.ID):
			row.Status = "COMPLETED"
			active = append(active, row)
		case pickedCampaignIDs[c.ID]:
			row.Status = "ACTIVE"
			row.QueueIndex = queueIdx
			queueIdx++
			if pick != nil {
				row.ChannelLogin = pick.ChannelLogin
			}
			active = append(active, row)
		case campaignsInPool[c.ID] != nil:
			row.Status = "QUEUED"
			row.QueueIndex = queueIdx
			queueIdx++
			queued = append(queued, row)
		default:
			row.Status = "IDLE"
			idle = append(idle, row)
		}
	}

	return active, queued, idle
}

// campaignToRow projects a DropCampaign into the ActiveDrop UI shape using
// the campaign's first unclaimed watchable drop for progress + ETA.
func campaignToRow(c twitch.DropCampaign, pinnedID string) ActiveDrop {
	var dropName string
	var progress, required int
	for _, d := range c.Drops {
		if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
			continue
		}
		dropName = d.BenefitName
		if dropName == "" {
			dropName = d.Name
		}
		progress = d.CurrentMinutesWatched
		required = d.RequiredMinutesWatched
		break
	}

	pct := 0
	if required > 0 {
		pct = (progress * 100) / required
		if pct > 100 {
			pct = 100
		}
	}

	eta := required - progress
	if eta < 0 {
		eta = 0
	}

	return ActiveDrop{
		CampaignID:         c.ID,
		CampaignName:       c.Name,
		GameName:           c.GameName,
		DropName:           dropName,
		Progress:           progress,
		Required:           required,
		Percent:            pct,
		EndAt:              c.EndAt,
		IsEnabled:          true,
		IsAccountConnected: c.IsAccountConnected,
		IsPinned:           c.ID == pinnedID && pinnedID != "",
		EtaMinutes:         eta,
	}
}

// applySelectorPick registers the picked channel as a temp channel if not
// already tracked, sets HasActiveDrop=true on it, and clears HasActiveDrop on
// any other channel that was the previous pick. Idempotent — called every cycle.
func (f *Farmer) applySelectorPick(pick *PoolEntry, campaigns []twitch.DropCampaign) {
	// Snapshot current pick before swap.
	f.drops.mu.RLock()
	prevPickID := f.drops.currentPickID
	f.drops.mu.RUnlock()

	if pick == nil {
		// Clear previous pick if any.
		if prevPickID != "" {
			f.mu.RLock()
			ch, ok := f.channels[prevPickID]
			f.mu.RUnlock()
			if ok {
				ch.ClearDropInfo()
			}
		}
		return
	}

	// Find or add the picked channel.
	f.mu.RLock()
	ch, exists := f.channels[pick.ChannelID]
	f.mu.RUnlock()

	if !exists {
		// Use the picked campaign's primary (Campaigns[0]) ID for tracking.
		primaryCampID := ""
		if len(pick.Campaigns) > 0 {
			primaryCampID = pick.Campaigns[0].ID
		}
		if err := f.addTemporaryChannel(pick.ChannelLogin, primaryCampID); err != nil {
			f.addLog("[Drops/Pool] failed to add %s: %v", pick.ChannelLogin, err)
			return
		}
		f.mu.RLock()
		ch = f.channels[pick.ChannelID]
		f.mu.RUnlock()
		if ch == nil {
			return
		}
	}

	// Set drop info on picked channel from its primary campaign's first unclaimed drop.
	primaryCampID := ""
	if len(pick.Campaigns) > 0 {
		primaryCampID = pick.Campaigns[0].ID
	}
	for _, c := range campaigns {
		if c.ID != primaryCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
				continue
			}
			name := d.BenefitName
			if name == "" {
				name = d.Name
			}
			ch.SetDropInfo(name, d.CurrentMinutesWatched, d.RequiredMinutesWatched)
			ch.mu.Lock()
			ch.CampaignID = c.ID
			ch.mu.Unlock()
			break
		}
		break
	}

	// Clear previous pick if it was a different channel.
	if prevPickID != "" && prevPickID != pick.ChannelID {
		f.mu.RLock()
		prevCh, ok := f.channels[prevPickID]
		f.mu.RUnlock()
		if ok {
			prevCh.ClearDropInfo()
		}
	}
}

// cleanupNonPickedTempChannels removes every temporary channel that is NOT
// the current pick. The previous pick (if it was temp) gets removed; any
// temp channels left over from earlier code paths get cleaned up too.
func (f *Farmer) cleanupNonPickedTempChannels(pick *PoolEntry) {
	pickID := ""
	if pick != nil {
		pickID = pick.ChannelID
	}

	f.mu.RLock()
	var stale []string
	for chID, ch := range f.channels {
		if ch.Snapshot().IsTemporary && chID != pickID {
			stale = append(stale, chID)
		}
	}
	f.mu.RUnlock()

	for _, chID := range stale {
		f.removeTemporaryChannel(chID)
	}
}
```

- [ ] **Step 7.5: Update `GetActiveDrops` to return concatenation**

Find `GetActiveDrops` near the bottom of `internal/farmer/drops.go` and replace it with:

```go
// GetActiveDrops returns the union of active+queued+idle drops for the web UI.
// Sorted: ACTIVE first, then QUEUED by index, then IDLE, then DISABLED/COMPLETED.
func (f *Farmer) GetActiveDrops() []ActiveDrop {
	f.drops.mu.RLock()
	defer f.drops.mu.RUnlock()

	total := len(f.drops.activeDrops) + len(f.drops.queuedDrops) + len(f.drops.idleDrops)
	if total == 0 {
		return nil
	}
	out := make([]ActiveDrop, 0, total)
	out = append(out, f.drops.activeDrops...)
	out = append(out, f.drops.queuedDrops...)
	out = append(out, f.drops.idleDrops...)
	return out
}
```

- [ ] **Step 7.6: Verify the file compiles**

Run: `go build ./internal/farmer/ 2>&1`

Expected: errors about undefined helpers from old code (e.g. `findAllowedChannelViaDirectory`) — those will be removed in Task 8. If errors are about the NEW code (typos, missing fields), fix those before continuing.

Acceptable now: undefined references to `handleDropFailover`, `verifyTempChannelHealth`, etc.

- [ ] **Step 7.7: Commit (without running build)**

```bash
git add internal/farmer/drops.go
git commit -m "farmer: rewrite processDrops with selector-driven channel pick"
```

---

## Task 8: Strip removed functions from `drops.go`

**Files:**
- Modify: `internal/farmer/drops.go`

- [ ] **Step 8.1: Identify functions to delete**

Run: `grep -n "^func.*(f \*Farmer)" internal/farmer/drops.go`

The following must be deleted (no other code in the repo should call them after Task 7):

- `findAllowedChannelViaDirectory`
- `findLiveFromAllowedChannels`
- `autoSelectDropChannel`
- `pickExclusiveCampaigns`
- `checkDropProgressStalls`
- `cleanupFailoverCooldowns`
- `cleanupTemporaryChannels` (replaced by `cleanupNonPickedTempChannels`)
- `spadeSlotsSaturated`
- `matchCampaignChannels`
- `verifyTempChannelHealth` (only the version inside drops.go if it's there; the one in farmer.go gets handled in Task 9)

Also delete the `farmingBenefitIDs` variable handling and old `dropStallCount` / `failoverCooldowns` map allocations if any leak from old initializers.

- [ ] **Step 8.2: Delete each function**

For each function in the list above, use a precise grep to locate it then remove the block from `func` line through the matching closing `}`. Example for `pickExclusiveCampaigns`:

```bash
# Locate
grep -n "^func.*pickExclusiveCampaigns" internal/farmer/drops.go
# Manually delete the function body (open the file, remove from `func` line to the closing `}` of the function).
```

If a function is referenced from `farmer.go`, that reference will need to go too — handle in Task 9.

- [ ] **Step 8.3: Verify compile + tests still pass**

Run: `go build ./... 2>&1`

Expected (if Task 9 hasn't been done yet): errors about `farmer.go` still referencing removed functions (`handleDropFailover`, etc.). That's fine — Task 9 handles it.

If errors appear in `drops.go` itself after deletion (e.g. unused imports), fix them: run `goimports -w internal/farmer/drops.go` if available, otherwise manually remove unused imports.

Run: `go test ./internal/farmer/ -run "TestFilterEligibleCampaigns|TestBuildPool|TestSortPool|TestSelect" -v`

Expected: all selector tests still PASS (they don't depend on the deleted code).

- [ ] **Step 8.4: Commit**

```bash
git add internal/farmer/drops.go
git commit -m "farmer: remove dead drops.go code superseded by selector"
```

---

## Task 9: Strip drop-related dead code from `farmer.go`

**Files:**
- Modify: `internal/farmer/farmer.go`

- [ ] **Step 9.1: Delete `handleDropFailover`**

Locate: `grep -n "func.*handleDropFailover" internal/farmer/farmer.go`

Delete the entire function body (and the inline `transferDrop` closure inside it).

- [ ] **Step 9.2: Delete `verifyTempChannelHealth`**

Locate: `grep -n "func.*verifyTempChannelHealth" internal/farmer/farmer.go`

Delete the entire function body. Also remove any caller of it (likely in `processDrops` from old code — but `processDrops` was already replaced in Task 7, so no caller should remain).

- [ ] **Step 9.3: Delete `findLiveFromGameDirectory` and `findLiveFromGameDirectoryExcluding`**

These were used by the failover logic. The selector queries `GetGameStreamsDropsEnabled` directly, so these wrappers are unused.

Locate and delete both functions.

- [ ] **Step 9.4: Verify compile**

Run: `go build ./... 2>&1`

Expected: exit 0. If errors:
- Unused imports: remove them
- Other unused private helpers: delete or leave (but don't break compilation)

Run: `go vet ./...`

Expected: exit 0.

- [ ] **Step 9.5: Run all tests**

Run: `go test ./...`

Expected: PASS for all packages.

- [ ] **Step 9.6: Commit**

```bash
git add internal/farmer/farmer.go
git commit -m "farmer: remove handleDropFailover and stale-detection helpers"
```

---

## Task 10: Add Pin endpoint to Web API

**Files:**
- Modify: `internal/web/server.go`

- [ ] **Step 10.1: Add route registration**

In `internal/web/server.go`, find `setupRoutes` (around line 39). The existing `/api/drops/` HandleFunc registers `handleDropAction`. Update `handleDropAction` (NOT `setupRoutes`) to dispatch on action.

Run: `grep -n "func.*handleDropAction\|/toggle\|action :=" internal/web/server.go`

- [ ] **Step 10.2: Update `handleDropAction` to support both `toggle` and `pin`**

Find the existing `handleDropAction` function (around line 299). Replace its body with:

```go
func (s *Server) handleDropAction(w http.ResponseWriter, r *http.Request) {
	// /api/drops/{campaignID}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/drops/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		jsonError(w, "invalid path, expected /api/drops/{campaignID}/{action}", http.StatusBadRequest)
		return
	}
	campaignID, action := parts[0], parts[1]

	switch action {
	case "toggle":
		s.handleCampaignToggle(w, r, campaignID)
	case "pin":
		s.handleCampaignPin(w, r, campaignID)
	default:
		jsonError(w, "unknown action: "+action, http.StatusBadRequest)
	}
}

func (s *Server) handleCampaignPin(w http.ResponseWriter, r *http.Request, campaignID string) {
	if r.Method != http.MethodPut {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	cfg := s.farmer.Config()
	if req.Pinned {
		cfg.SetPinnedCampaign(campaignID)
	} else if cfg.IsCampaignPinned(campaignID) {
		cfg.SetPinnedCampaign("")
	}
	if err := cfg.Save(); err != nil {
		jsonError(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{
		"pinned_campaign_id": cfg.GetPinnedCampaign(),
	})
}
```

- [ ] **Step 10.3: Extract the existing toggle logic into `handleCampaignToggle`**

In `internal/web/server.go`, the original `handleDropAction` body (before Step 10.2's replacement) contained the toggle logic. Move that into a new helper:

```go
func (s *Server) handleCampaignToggle(w http.ResponseWriter, r *http.Request, campaignID string) {
	// PASTE THE ORIGINAL TOGGLE BODY HERE (the part that was originally inside handleDropAction
	// after the path parsing but before any pin support)
}
```

Run: `git diff internal/web/server.go` to see what was there originally; reconstruct from the diff if needed.

- [ ] **Step 10.4: Add `Config()` accessor on Farmer if missing**

Run: `grep -n "func.*Farmer.*Config()" internal/farmer/*.go`

If no such accessor exists, add to `internal/farmer/farmer.go` (just before `addLog` or some other public method):

```go
// Config returns the farmer's configuration. Used by the web layer for pin/disable mutations.
func (f *Farmer) Config() *config.Config {
	return f.cfg
}
```

- [ ] **Step 10.5: Verify compile**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 10.6: Smoke-test endpoint manually with curl (after binary rebuild later — for now just verify routes register)**

Run: `go test ./internal/web/ 2>&1 | head -20`

If web has no tests, expected: `?       internal/web    [no test files]`. That's fine.

- [ ] **Step 10.7: Commit**

```bash
git add internal/web/server.go internal/farmer/farmer.go
git commit -m "web: add PUT /api/drops/{id}/pin endpoint and Farmer.Config accessor"
```

---

## Task 11: Update Web UI HTML for Pin column + Status

**Files:**
- Modify: `internal/web/static/index.html`

The existing channels-table render uses `innerHTML` with values that may originate from Twitch (channel names, game names). For v1.7.0 we add an `escapeHTML` helper and use it for all user/Twitch-supplied strings in the new drops-table renderer. This is defense-in-depth — Twitch could (in theory) include HTML in a campaign name.

- [ ] **Step 11.1: Locate the drops table in the HTML**

Run: `grep -n "Drop Campaigns\|<th>Status\|<th>Game\|<th>Channel\|api/drops" internal/web/static/index.html`

The drops table starts at line 542 (`<h2>Drop Campaigns</h2>`). Note the structure: `<table>` with `<thead>` and `<tbody id="...">`.

- [ ] **Step 11.2: Find the table headers and JavaScript that populates it**

Read lines around the drops table:

Run: `sed -n '540,610p' internal/web/static/index.html`

Identify:
- The `<th>` row (column headers)
- The `<tbody>` ID (e.g. `dropsTbody`)
- The JS function that fetches `/api/drops` and renders rows (around line 724)

- [ ] **Step 11.3: Update column headers**

In the drops `<thead>`, replace the existing `<tr>` of `<th>` with:

```html
<tr>
    <th>📌</th>
    <th>#</th>
    <th>Status</th>
    <th>Campaign</th>
    <th>Game</th>
    <th>Progress</th>
    <th>ETA</th>
    <th>Channel</th>
    <th>Actions</th>
</tr>
```

(Keep whatever surrounding `<tr>`/`<thead>` structure exists — only replace the inner cells.)

- [ ] **Step 11.4: Add escapeHTML helper near the top of the existing `<script>` block**

Find the opening `<script>` tag (search via `grep -n "<script>" internal/web/static/index.html`). Add inside, near the top of the script:

```javascript
function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}
```

- [ ] **Step 11.5: Update the JS render function**

Run: `grep -n "dropsTbody\|/api/drops'" internal/web/static/index.html`

Find the JavaScript function that builds rows from the `/api/drops` response. Replace its row-building loop body to produce the new columns. Note all dynamic Twitch-sourced strings (`campaign_name`, `game_name`, `channel_login`, `campaign_id`) go through `escapeHTML`. Booleans and numbers are safe to interpolate raw:

```javascript
function renderDrops(drops) {
    const tbody = document.getElementById('dropsTbody'); // adapt name to actual ID
    if (!drops || drops.length === 0) {
        tbody.innerHTML = '<tr><td colspan="9" style="text-align:center;color:#888">No active drops</td></tr>';
        return;
    }
    tbody.innerHTML = drops.map(d => {
        const pinIcon = d.is_pinned ? '📌✓' : '📌';
        const statusClass = String(d.status || '').toLowerCase().replace(/[^a-z]/g, '');
        const statusBadge = `<span class="status-${statusClass}">${escapeHTML(d.status)}</span>`;
        const queuePos = d.queue_index > 0 ? d.queue_index : '—';
        const etaMin = d.eta_minutes;
        const etaStr = etaMin <= 0 ? '—' : (etaMin < 60 ? `${etaMin}m` : `~${(etaMin / 60).toFixed(1)}h`);
        const channel = d.channel_login ? escapeHTML(d.channel_login) : '—';
        const progressStr = `${d.progress}/${d.required} (${d.percent}%)`;
        const isCompleted = d.status === 'COMPLETED';
        const isDisabled = d.status === 'DISABLED';
        const safeCampId = escapeHTML(d.campaign_id);

        const actions = isCompleted
            ? ''
            : `<button onclick="togglePin('${safeCampId}', ${!d.is_pinned})">${pinIcon}</button>
               <button onclick="toggleCampaign('${safeCampId}', ${isDisabled})">${isDisabled ? '↺' : '✕'}</button>`;

        return `<tr>
            <td>${d.is_pinned ? '📌' : ''}</td>
            <td>${queuePos}</td>
            <td>${statusBadge}</td>
            <td>${escapeHTML(d.campaign_name)}</td>
            <td>${escapeHTML(d.game_name)}</td>
            <td>${progressStr}</td>
            <td>${etaStr}</td>
            <td>${channel}</td>
            <td>${actions}</td>
        </tr>`;
    }).join('');
}

async function togglePin(campaignId, pinned) {
    const res = await fetch(`/api/drops/${encodeURIComponent(campaignId)}/pin`, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({pinned})
    });
    if (!res.ok) {
        alert('Pin failed: ' + (await res.text()));
        return;
    }
    refreshDrops();
}
```

(Adapt the existing `toggleCampaign` function reference — find it via `grep "toggleCampaign\|api/drops.*toggle" internal/web/static/index.html` and keep it. Note `safeCampId` is the escaped form; it's used as a JS string literal inside the template — escapeHTML neutralizes both HTML and quote injection here.)

- [ ] **Step 11.6: Add CSS for status badges**

In the existing `<style>` block (search for `<style>`), add:

```css
.status-active   { color: #2ecc40; font-weight: bold; }
.status-queued   { color: #ffc107; }
.status-idle     { color: #888; font-style: italic; }
.status-disabled { color: #777; text-decoration: line-through; }
.status-completed{ color: #4a90e2; }
```

- [ ] **Step 11.7: Manual visual check**

(Cannot test automatically without the bot running. Defer to Task 13 for full smoke test.)

Run: `go build ./...`

Expected: exit 0 (HTML changes don't affect Go compilation, just confirms nothing else broke).

- [ ] **Step 11.8: Commit**

```bash
git add internal/web/static/index.html
git commit -m "web: drop campaigns table — pin button, status badges, queue index, escapeHTML"
```

---

## Task 12: Version bump + changelog + build

**Files:**
- Modify: `cmd/twitchpoint/main.go`
- Modify: `changelog.txt`
- Build: all platforms

- [ ] **Step 12.1: Bump version**

In `cmd/twitchpoint/main.go`, change:

```go
const appVersion = "1.6.5"
```

to:

```go
const appVersion = "1.7.0"
```

- [ ] **Step 12.2: Add changelog entry**

Get current timestamp: `date "+%Y-%m-%d %H:%M"`.

Prepend to `changelog.txt` (top of file, before existing v1.6.5 entry):

```
== <YYYY-MM-DD HH:MM> == Major: Drop subsystem rewrite — channel-pool selector (v1.7.0) ==

Replaces ~1500 LoC of parallel-multi-drop coordination (drops.go +
farmer.go failover/stall machinery) with a ~250 LoC single channel-pool
selector. Inspired by TwitchDropsMiner.

What changed:
- New file internal/farmer/dropselector.go: pure-function pipeline
  filter → buildPool → sort → pick. Pool dedupes channels that serve
  multiple campaigns.
- internal/farmer/drops.go rewritten: ~400 LoC slim coordinator that
  polls inventory, runs selector, applies pick to Spade slot 1 via the
  existing HasActiveDrop/rotateChannels mechanism.
- handleDropFailover, transferDrop, verifyTempChannelHealth (the
  stale-detection part), checkDropProgressStalls, pickExclusiveCampaigns,
  findAllowedChannelViaDirectory, findLiveFromAllowedChannels,
  findLiveFromGameDirectory, findLiveFromGameDirectoryExcluding,
  spadeSlotsSaturated, matchCampaignChannels — all removed. Every cycle
  now picks fresh from the live drops-enabled pool, so failover is
  implicit (next cycle, next pick).
- New config field PinnedCampaignID with helpers
  SetPinnedCampaign / IsCampaignPinned / GetPinnedCampaign. Backward
  compatible — old configs load with empty pin.
- New PUT /api/drops/{id}/pin endpoint. Web UI gains pin button per
  campaign, Status column (ACTIVE / QUEUED / IDLE / DISABLED /
  COMPLETED), QueueIndex column, ETA column.
- escapeHTML helper added to drops-table JS render path (defense in
  depth: Twitch-supplied strings now properly escaped).
- Spade slot allocation unchanged: P0 (HasActiveDrop) wins slot 1,
  slot 2 follows existing P1 always-watch / P2 rotate rotation.
- Channel-points farming unaffected.

Why: v1.5.x → v1.6.5 produced a chain of fixes (exclusive dedup, failover
state transfer, drops-enabled filter, stall failover, "pick yourself as
replacement" bug) where each fix exposed the next sync bug between
per-campaign and per-channel state machines. Collapsing to one channel
pick per cycle removes the synchronization surface entirely.

Net: ~450 LoC removed, all selector logic table-driven unit-tested.

Files: internal/config/config.go (+15), internal/farmer/dropselector.go
(new ~250), internal/farmer/drops.go (rewritten ~400, was 958),
internal/farmer/farmer.go (~-220), internal/web/server.go (+~60),
internal/web/static/index.html (~50 line table layout change),
cmd/twitchpoint/main.go (1.6.5 → 1.7.0), tests in
internal/config/config_test.go (new) and
internal/farmer/dropselector_test.go (new).

```

(Replace `<YYYY-MM-DD HH:MM>` with the actual `date` output.)

- [ ] **Step 12.3: Run full test suite**

Run: `go test ./...`

Expected: all tests PASS. If anything fails, fix before continuing.

- [ ] **Step 12.4: Build all platform binaries**

Run: `make build-all`

Expected output:
```
GOOS=darwin  GOARCH=arm64 go build -o bin/twitchpoint-macos       ./cmd/twitchpoint
GOOS=linux   GOARCH=amd64 go build -o bin/twitchpoint-linux       ./cmd/twitchpoint
GOOS=windows GOARCH=amd64 go build -o bin/twitchpoint-windows.exe ./cmd/twitchpoint
```

Verify binaries:

```bash
ls -la bin/twitchpoint-*
```

Expected: 3 binaries with current timestamp.

- [ ] **Step 12.5: Backup v1.6.5 binary before swap**

Run:

```bash
cp bin/twitchpoint-macos bin/twitchpoint-macos.bak.v1.6.5 2>/dev/null || true
```

(Ignore failure if v1.6.5 binary was already overwritten — the new builds are what we deploy.)

- [ ] **Step 12.6: Commit**

```bash
git add cmd/twitchpoint/main.go changelog.txt bin/twitchpoint-macos bin/twitchpoint-linux bin/twitchpoint-windows.exe
git commit -m "$(cat <<'EOF'
release: v1.7.0 — drop subsystem channel-pool rewrite

See changelog for full detail. ~450 LoC removed, all selector logic
unit-tested. Backward-compatible config.
EOF
)"
```

---

## Task 13: Manual smoke test against running bot

**Files:** none (verification only)

This is a manual checklist the human runs against the live bot after restarting with the new binary.

- [ ] **Step 13.1: Stop the running v1.6.5 bot**

User action: Ctrl+C the running bot, or `pkill -f twitchpoint-macos`.

Confirm: `ps aux | grep twitchpoint | grep -v grep` returns nothing.

- [ ] **Step 13.2: Start v1.7.0 bot**

Run: `./bin/twitchpoint-macos &` (or however the user usually starts it).

Tail logs: `tail -f logs/debug-$(date +%Y-%m-%d).log | grep -E "Drops/Pool|Farmer started"`

Expected within ~30 seconds:
```
[<time>] === TwitchPoint Farmer started ===
[<time>] [Drops/Pool] picked <some_streamer> (campaigns: <campaign_name>)
```

If `[Drops/Pool] empty pool` appears instead, that means no live drops-enabled streamers exist for any eligible campaign — check inventory. This is a valid state, not a bug.

- [ ] **Step 13.3: Verify Spade slot is the picked channel**

Run:

```bash
PORT=$(jq -r '.web_port // 8080' config.json)
curl -s http://localhost:${PORT}/api/channels | jq '[.[] | select(.is_watching == true) | {login, has_active_drop, is_temporary}]'
```

Expected: at least one watching channel with `has_active_drop: true` matching the pool pick from Step 13.2 logs.

- [ ] **Step 13.4: Verify drops API response shape**

Run:

```bash
curl -s http://localhost:${PORT}/api/drops | jq '.[0]'
```

Expected: object with `status`, `is_pinned`, `queue_index`, `eta_minutes` fields populated. The first entry should have `status: "ACTIVE"` and a non-empty `channel_login`.

- [ ] **Step 13.5: Test pin endpoint**

Pick a non-active campaign ID from `/api/drops` (one with `status: "QUEUED"` or `IDLE`):

```bash
CID=$(curl -s http://localhost:${PORT}/api/drops | jq -r '.[] | select(.status=="QUEUED") | .campaign_id' | head -1)
curl -s -X PUT http://localhost:${PORT}/api/drops/${CID}/pin -H 'Content-Type: application/json' -d '{"pinned":true}' | jq .
```

Expected: `{"pinned_campaign_id": "<CID>"}`.

Wait one inventory cycle (5 min) or trigger an immediate run by toggling drops_enabled. Then check:

```bash
curl -s http://localhost:${PORT}/api/drops | jq '.[] | select(.is_pinned)'
```

Expected: the pinned campaign now has `status: "ACTIVE"` (or stays QUEUED if its game has no live drops-enabled streams — check pool log).

- [ ] **Step 13.6: Web UI visual check**

Open `http://localhost:${PORT}/` in a browser.

Verify:
- Drops table has Pin column (📌), Status column with badges
- Active drop row is highlighted/colored differently from queued
- Pin button works (icon flips after click)
- Disabling/re-enabling a campaign still works

- [ ] **Step 13.7: Wait one full cycle and check progress**

Wait 5-10 minutes. Run:

```bash
curl -s http://localhost:${PORT}/api/drops | jq '.[] | select(.status=="ACTIVE") | {campaign: .campaign_name, channel: .channel_login, progress: "\(.progress)/\(.required)"}'
```

Expected: progress > 0. If still 0 after 2 cycles, that's the same Twitch-side issue from before (drops-enabled streamer not crediting); selector should naturally pick a different channel next cycle if pool size > 1.

- [ ] **Step 13.8: Verify no regression in channel-points farming**

Run:

```bash
grep "+10 points" logs/debug-$(date +%Y-%m-%d).log | tail -5
```

Expected: regular `+10 points on <channel> (WATCH)` entries appearing. If completely absent, channel-points farming broke — check `rotateChannels` integration.

- [ ] **Step 13.9: If everything green, mark v1.7.0 release confirmed**

Append to changelog (or open a follow-up commit):

```
Verified live: v1.7.0 bot picked <channel> for <campaign>, progress ticked
on first cycle, pin endpoint switched correctly.
```

---

## Self-Review

**Spec coverage:**
- ✅ Channel-pool selector → Tasks 2-5
- ✅ Pinning → Task 1 (config) + Task 10 (endpoint) + Task 11 (UI)
- ✅ Drops.go rewrite → Tasks 6-8
- ✅ farmer.go cleanup → Task 9
- ✅ Web API extension → Task 10
- ✅ Web UI changes (with XSS escaping as a defense-in-depth bonus) → Task 11
- ✅ Migration plan → Task 12 (bump + build, config backward compat already covered by Task 1)
- ✅ Acceptance criteria → Task 13 (manual smoke test against the criteria from spec)

**Placeholders:** None. Every code block is concrete. Two places use `<placeholder>`:
- Task 12.2: `<YYYY-MM-DD HH:MM>` in changelog — instruction says to replace via `date` command output.
- Task 13.2: `<time>` and `<some_streamer>` — those are LITERAL placeholders in expected log output, the user reads them off when verifying.

These are not plan placeholders (which would be undefined behavior); they're literal text the human substitutes from runtime values. Acceptable.

**Type/method consistency:**
- `PoolEntry`, `CampaignRef`, `DropSelector` defined in Task 2, used consistently in Tasks 3-7.
- `SetPinnedCampaign` / `GetPinnedCampaign` / `IsCampaignPinned` defined in Task 1, used in Tasks 5 (selector test), 7 (drops.go), 10 (web).
- `ActiveDrop.Status` etc. defined in Task 6, used in Task 7 (rows) and Task 11 (UI).
- `cleanupNonPickedTempChannels` defined in Task 7.4, called from `processDrops` in Task 7.3 — consistent name.
- `Farmer.Config()` accessor added in Task 10.4 if missing — used in Task 10.2's pin handler.
- `escapeHTML` defined in Task 11.4, used in Task 11.5 — same file scope.

**Note for the executor:** if `internal/web/server.go` or `internal/farmer/farmer.go` looks structurally different from what the plan assumes (e.g. fields are private under different names), use the noted `grep` commands in early steps to discover the real names and adapt — the plan's pattern is what matters, not the exact identifier strings.
