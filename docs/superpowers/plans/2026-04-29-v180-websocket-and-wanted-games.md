# v1.8.0 — WebSocket + `wanted_games` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace 5-min inventory polling with Twitch PubSub WebSocket for real-time drop progress + game-change detection, and replace `remaining_time` primary sort with a user-ordered `wanted_games` priority list.

**Architecture:** Subscribe to `user-drop-events.<userID>` (drop progress + claims) at startup; subscribe to `broadcast-settings-update.<channelID>` per currently-watched channel (game switch detection). Selector sorts pool by `(wanted_games rank, earliest endAt, viewer count)` with empty-list fallback to v1.7.0 `remaining_time` behavior. TUI gains a modal game-list editor on `g`. Polling reduced from 5 min to 15 min as safety net.

**Tech Stack:** Go 1.22+, no new dependencies. Existing `internal/twitch/pubsub.go` (gorilla/websocket) extended with new topic patterns + Unlisten method. Tests use Go's built-in `testing` package.

**Spec:** `docs/superpowers/specs/2026-04-29-v180-websocket-and-wanted-games.md`

**Branch:** main (project allows direct main commits per user CLAUDE.md).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/config/config.go` | Modify | Add `GamesToWatch` field + 5 helpers; add `UnmarkCampaignCompleted` |
| `internal/config/config_test.go` | Modify | Tests for new helpers |
| `internal/twitch/types.go` | Modify | Add 3 new FarmerEventType constants + payload types |
| `internal/twitch/pubsub.go` | Modify | Add `Unlisten` method + 2 new topic-prefix handlers |
| `internal/farmer/dropselector.go` | Modify | `sortPool` uses wanted_games rank (with empty-list fallback) |
| `internal/farmer/dropselector_test.go` | Modify | Tests for wanted_games sort + empty-list fallback |
| `internal/farmer/drops.go` | Modify | New `scrubStaleCompleted` + `applyDropProgressUpdate` + per-channel topic delta in `applySelectorPick` |
| `internal/farmer/farmer.go` | Modify | Handlers for new events; subscribe `user-drop-events` at start; cadence 5→15 min |
| `internal/ui/app.go` | Modify | New `inputGameList` modal state + `g` keybind + editor key dispatch |
| `internal/ui/components.go` | Modify | New `renderGameListEditor` overlay; help bar gains `g games` (loses `i`) |
| `internal/web/server.go` | Modify | New `GET/PUT /api/wanted_games` endpoints |
| `internal/web/static/index.html` | Modify | New "Wanted Games" section with reorder UI; remove pin button |
| `cmd/twitchpoint/main.go` | Modify | Version 1.7.0 → 1.8.0 |
| `changelog.txt` | Modify | New entry at top |

**Decomposition:** Selector + config helpers are pure & TDD-tested. PubSub plumbing + event handlers are integration code, smoke-tested manually against the live bot. UI is visual-tested manually.

---

## Task 1: Config — `GamesToWatch` + helpers + `UnmarkCampaignCompleted`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1.1: Add `GamesToWatch` field to Config struct**

In `internal/config/config.go`, find the existing `PinnedCampaignID` line (added in v1.7.0). Add immediately after it:

```go
GamesToWatch []string `json:"games_to_watch,omitempty"` // ordered priority list of game names; empty = remaining_time fallback
```

- [ ] **Step 1.2: Add helpers at end of file**

Append after the existing `GetPinnedCampaign` helper:

```go
// GetGamesToWatch returns the ordered wanted-games list (copy, safe to mutate).
func (c *Config) GetGamesToWatch() []string {
	out := make([]string, len(c.GamesToWatch))
	copy(out, c.GamesToWatch)
	return out
}

// AddGameToWatch appends a game to the end of the priority list if not already present.
// Comparison is case-insensitive.
func (c *Config) AddGameToWatch(game string) {
	game = strings.TrimSpace(game)
	if game == "" {
		return
	}
	for _, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			return
		}
	}
	c.GamesToWatch = append(c.GamesToWatch, game)
}

// RemoveGameFromWatch removes a game from the priority list (case-insensitive).
func (c *Config) RemoveGameFromWatch(game string) {
	for i, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			c.GamesToWatch = append(c.GamesToWatch[:i], c.GamesToWatch[i+1:]...)
			return
		}
	}
}

// MoveGameToWatch shifts a game one position in the priority list.
// direction: -1 = up (toward higher priority), +1 = down. No-op at boundaries.
func (c *Config) MoveGameToWatch(game string, direction int) {
	idx := -1
	for i, g := range c.GamesToWatch {
		if strings.EqualFold(g, game) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	target := idx + direction
	if target < 0 || target >= len(c.GamesToWatch) {
		return
	}
	c.GamesToWatch[idx], c.GamesToWatch[target] = c.GamesToWatch[target], c.GamesToWatch[idx]
}

// SetGamesToWatch replaces the whole list (used by web API atomic reorder).
func (c *Config) SetGamesToWatch(games []string) {
	out := make([]string, 0, len(games))
	seen := make(map[string]bool, len(games))
	for _, g := range games {
		key := strings.ToLower(strings.TrimSpace(g))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, strings.TrimSpace(g))
	}
	c.GamesToWatch = out
}

// UnmarkCampaignCompleted removes a campaign ID from the completed list.
// Used by daily-rolling-campaign scrub when Twitch resets a campaign's drops.
func (c *Config) UnmarkCampaignCompleted(campaignID string) {
	for i, id := range c.CompletedCampaigns {
		if id == campaignID {
			c.CompletedCampaigns = append(c.CompletedCampaigns[:i], c.CompletedCampaigns[i+1:]...)
			return
		}
	}
}
```

- [ ] **Step 1.3: Add tests**

Append to `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 1.4: Run tests, verify pass**

Run: `go test ./internal/config/ -v`

Expected: PASS for all four new tests + the existing v1.7.0 tests (Pin tests).

- [ ] **Step 1.5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: add GamesToWatch ordered list + UnmarkCampaignCompleted"
```

---

## Task 2: Twitch event types — `EventDropProgress`, `EventDropClaim`, `EventGameChange`

**Files:**
- Modify: `internal/twitch/types.go`

- [ ] **Step 2.1: Add new event-type constants**

In `internal/twitch/types.go`, find the existing block of FarmerEventType constants (e.g. `EventClaimAvailable`, `EventStreamUp`, etc.). Append (KEEP iota order — these are new values appended at the end):

```go
	// v1.8.0 WebSocket-driven events
	EventDropProgress // user-drop-events: a drop's currentMinutesWatched changed
	EventDropClaim    // user-drop-events: a drop instance is ready to claim or was claimed
	EventGameChange   // broadcast-settings-update: a watched channel changed game/title
```

- [ ] **Step 2.2: Add payload structs at end of types.go**

Append:

```go
// DropProgressData is the payload for EventDropProgress.
type DropProgressData struct {
	CampaignID             string
	DropID                 string
	CurrentMinutesWatched  int
	RequiredMinutesWatched int
}

// DropClaimData is the payload for EventDropClaim.
type DropClaimData struct {
	CampaignID     string
	DropID         string
	DropInstanceID string // empty if Twitch is just notifying that a drop is now claimable
}

// GameChangeData is the payload for EventGameChange.
type GameChangeData struct {
	OldGameName string
	NewGameName string
	Title       string
}
```

- [ ] **Step 2.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 2.4: Commit**

```bash
git add internal/twitch/types.go
git commit -m "twitch: add v1.8.0 WebSocket event types and payloads"
```

---

## Task 3: PubSub — `Unlisten` method

**Files:**
- Modify: `internal/twitch/pubsub.go`

- [ ] **Step 3.1: Find existing `Listen` method as template**

Run: `grep -n "func.*PubSubClient.*Listen\b\|sendListen\|sendUnlisten" internal/twitch/pubsub.go`

Note the `Listen` method line; `Unlisten` will mirror it.

- [ ] **Step 3.2: Add `Unlisten` method**

In `internal/twitch/pubsub.go`, find the `Listen` method. Immediately after it, add:

```go
// Unlisten unsubscribes from one or more topics. Safe to call with topics
// that were never subscribed (Twitch's UNLISTEN frame ignores unknown topics).
// Used by v1.8.0 to drop per-watched-channel subscriptions when the selector
// switches to a different channel.
func (p *PubSubClient) Unlisten(topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	p.mu.Lock()
	for _, t := range topics {
		delete(p.topics, t)
	}
	p.mu.Unlock()

	if p.conn == nil {
		return nil // not connected yet, just removed from cache
	}

	const batchSize = 50
	for i := 0; i < len(topics); i += batchSize {
		end := i + batchSize
		if end > len(topics) {
			end = len(topics)
		}
		if err := p.sendUnlisten(topics[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// sendUnlisten writes an UNLISTEN frame for a batch of topics.
func (p *PubSubClient) sendUnlisten(topics []string) error {
	nonce, _ := generateNonce()
	frame := map[string]interface{}{
		"type":  "UNLISTEN",
		"nonce": nonce,
		"data": map[string]interface{}{
			"topics":     topics,
			"auth_token": p.authToken,
		},
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal unlisten: %w", err)
	}
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.conn.WriteMessage(websocket.TextMessage, body)
}
```

> Note: this assumes `generateNonce` and the `connMu`/`conn`/`topics`/`authToken` fields exist (they do — used by `sendListen`). If field names differ, run `grep "p\\..*conn\\|p\\..*topics\\|p\\..*authToken\\|p\\..*connMu" internal/twitch/pubsub.go | head -10` and adjust.

- [ ] **Step 3.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 3.4: Commit**

```bash
git add internal/twitch/pubsub.go
git commit -m "twitch: PubSubClient.Unlisten for per-channel topic removal"
```

---

## Task 4: PubSub — handle `user-drop-events` and `broadcast-settings-update` topics

**Files:**
- Modify: `internal/twitch/pubsub.go`

- [ ] **Step 4.1: Find the handleMessage topic dispatch**

Run: `grep -n "handleMessage\|case strings.HasPrefix" internal/twitch/pubsub.go | head -10`

Note the `handleMessage` switch.

- [ ] **Step 4.2: Add two new topic-prefix cases to handleMessage**

In `internal/twitch/pubsub.go`, find the existing switch's last case (e.g. `case strings.HasPrefix(topic, "raid."):`). Add the two new cases BEFORE the closing brace of the switch:

```go
case strings.HasPrefix(topic, "user-drop-events."):
    p.handleDropEvent(data.Message)
case strings.HasPrefix(topic, "broadcast-settings-update."):
    channelID := strings.TrimPrefix(topic, "broadcast-settings-update.")
    p.handleBroadcastSettings(channelID, data.Message)
```

- [ ] **Step 4.3: Add handler functions at end of file**

Append to `internal/twitch/pubsub.go`:

```go
// handleDropEvent parses user-drop-events messages.
// Twitch sends two relevant types: drop-progress and drop-claim.
// Schema:
//   { "type": "drop-progress",
//     "data": { "drop_id": "...", "campaign_id": "...",
//               "current_progress_min": 5, "required_progress_min": 60 } }
//   { "type": "drop-claim",
//     "data": { "drop_id": "...", "campaign_id": "...",
//               "drop_instance_id": "..." } }
func (p *PubSubClient) handleDropEvent(rawMessage string) {
	var msg struct {
		Type string `json:"type"`
		Data struct {
			DropID              string `json:"drop_id"`
			CampaignID          string `json:"campaign_id"`
			CurrentProgressMin  int    `json:"current_progress_min"`
			RequiredProgressMin int    `json:"required_progress_min"`
			DropInstanceID      string `json:"drop_instance_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawMessage), &msg); err != nil {
		return // ignore malformed
	}

	switch msg.Type {
	case "drop-progress":
		p.events <- FarmerEvent{
			Type: EventDropProgress,
			Data: DropProgressData{
				CampaignID:             msg.Data.CampaignID,
				DropID:                 msg.Data.DropID,
				CurrentMinutesWatched:  msg.Data.CurrentProgressMin,
				RequiredMinutesWatched: msg.Data.RequiredProgressMin,
			},
		}
	case "drop-claim":
		p.events <- FarmerEvent{
			Type: EventDropClaim,
			Data: DropClaimData{
				CampaignID:     msg.Data.CampaignID,
				DropID:         msg.Data.DropID,
				DropInstanceID: msg.Data.DropInstanceID,
			},
		}
	}
}

// handleBroadcastSettings parses broadcast-settings-update messages.
// Twitch sends:
//   { "type": "broadcast_settings_update",
//     "channel_id": "...",
//     "old_status": "...", "status": "...",
//     "old_game": "Arena Breakout: Infinite",
//     "game": "Just Chatting" }
func (p *PubSubClient) handleBroadcastSettings(channelID, rawMessage string) {
	var msg struct {
		OldGame string `json:"old_game"`
		Game    string `json:"game"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(rawMessage), &msg); err != nil {
		return
	}
	p.events <- FarmerEvent{
		ChannelID: channelID,
		Type:      EventGameChange,
		Data: GameChangeData{
			OldGameName: msg.OldGame,
			NewGameName: msg.Game,
			Title:       msg.Status,
		},
	}
}
```

- [ ] **Step 4.4: Verify build**

Run: `go build ./...`

Expected: exit 0. If `FarmerEvent` field names differ from `Type`/`Data`/`ChannelID`, run `grep "type FarmerEvent struct" -A 10 internal/twitch/types.go` and adjust.

- [ ] **Step 4.5: Commit**

```bash
git add internal/twitch/pubsub.go
git commit -m "twitch: PubSub handlers for user-drop-events and broadcast-settings-update"
```

---

## Task 5: Selector — wanted_games sort + tests

**Files:**
- Modify: `internal/farmer/dropselector.go`
- Modify: `internal/farmer/dropselector_test.go`

- [ ] **Step 5.1: Replace sortPool body with wanted_games-aware version**

In `internal/farmer/dropselector.go`, find the `sortPool` function. Replace its entire body with:

```go
func (s *DropSelector) sortPool(pool []*PoolEntry) {
	wanted := s.cfg.GetGamesToWatch()
	gameRanks := make(map[string]int, len(wanted))
	for i, g := range wanted {
		gameRanks[strings.ToLower(strings.TrimSpace(g))] = i
	}
	useGameSort := len(wanted) > 0
	notWantedRank := len(wanted) // sorts after all wanted games

	type cached struct {
		gameRank int
		minEnd   time.Time
	}
	keys := make(map[*PoolEntry]cached, len(pool))
	for _, e := range pool {
		var c cached
		c.gameRank = notWantedRank
		first := true
		for _, ref := range e.Campaigns {
			if useGameSort {
				if r, ok := gameRanks[strings.ToLower(ref.GameName)]; ok && r < c.gameRank {
					c.gameRank = r
				}
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
		// Primary: wanted-game rank (only if list non-empty)
		if useGameSort && ki.gameRank != kj.gameRank {
			return ki.gameRank < kj.gameRank
		}
		// Secondary: earlier endAt first
		if !ki.minEnd.Equal(kj.minEnd) {
			return ki.minEnd.Before(kj.minEnd)
		}
		// Tertiary: higher viewers first (tie-break)
		return pool[i].ViewerCount > pool[j].ViewerCount
	})

	// Reorder each entry's Campaigns list by endAt only (pin support removed in v1.8.0).
	for _, e := range pool {
		sort.SliceStable(e.Campaigns, func(i, j int) bool {
			return e.Campaigns[i].EndAt.Before(e.Campaigns[j].EndAt)
		})
	}
}
```

- [ ] **Step 5.2: Run existing tests, expect TestSortPool_PinnedFirst to fail**

Run: `go test ./internal/farmer/ -run "TestSortPool" -v`

Expected: `TestSortPool_PinnedFirst` FAILS (pin no longer affects sort), other sort tests PASS.

- [ ] **Step 5.3: Replace TestSortPool_PinnedFirst with wanted_games tests**

In `internal/farmer/dropselector_test.go`, find `TestSortPool_PinnedFirst` and replace it with:

```go
func TestSortPool_WantedGamesPriority(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"Game A", "Game B"} // A is rank 0, B is rank 1
	sel := newTestSelector(cfg)

	a := &PoolEntry{ChannelLogin: "for-a", ViewerCount: 100, Campaigns: []CampaignRef{
		{GameName: "Game A", EndAt: testNow.Add(20 * time.Hour)},
	}}
	b := &PoolEntry{ChannelLogin: "for-b", ViewerCount: 1000, Campaigns: []CampaignRef{
		{GameName: "Game B", EndAt: testNow.Add(2 * time.Hour)}, // closer expiry
	}}

	pool := []*PoolEntry{b, a}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "for-a" {
		t.Fatalf("Game A channel should win regardless of viewers/expiry, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_NotInWantedSortsLast(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"Wanted Game"}
	sel := newTestSelector(cfg)

	wanted := &PoolEntry{ChannelLogin: "wanted", Campaigns: []CampaignRef{
		{GameName: "Wanted Game", EndAt: testNow.Add(20 * time.Hour)},
	}}
	other := &PoolEntry{ChannelLogin: "other", Campaigns: []CampaignRef{
		{GameName: "Some Other Game", EndAt: testNow.Add(2 * time.Hour)},
	}}

	pool := []*PoolEntry{other, wanted}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "wanted" {
		t.Fatalf("wanted-game channel should sort first, got %s", pool[0].ChannelLogin)
	}
	if pool[1].ChannelLogin != "other" {
		t.Fatalf("non-wanted should sort after wanted, got %s", pool[1].ChannelLogin)
	}
}

func TestSortPool_EmptyWantedFallsBackToRemainingTime(t *testing.T) {
	cfg := &config.Config{} // no wanted_games — fallback to remaining_time
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
		t.Fatalf("with empty wanted_games, near-expiry should win (v1.7.0 fallback), got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_MultiCampaignChannelUsesBestGameRank(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"High", "Low"}
	sel := newTestSelector(cfg)

	multi := &PoolEntry{ChannelLogin: "multi", Campaigns: []CampaignRef{
		{GameName: "Low", EndAt: testNow.Add(5 * time.Hour)},
		{GameName: "High", EndAt: testNow.Add(20 * time.Hour)}, // best game-rank for this channel
	}}
	lowOnly := &PoolEntry{ChannelLogin: "lowOnly", Campaigns: []CampaignRef{
		{GameName: "Low", EndAt: testNow.Add(2 * time.Hour)},
	}}

	pool := []*PoolEntry{lowOnly, multi}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "multi" {
		t.Fatalf("multi-campaign channel should win because it covers High game, got %s", pool[0].ChannelLogin)
	}
}
```

Also UPDATE `TestSelect_PinForcesNonClosestExpiry` — it's now obsolete because pin is removed. Delete it (search for it and remove the whole function).

- [ ] **Step 5.4: Run tests, verify pass**

Run: `go test ./internal/farmer/ -run "TestSortPool|TestSelect" -v`

Expected: All sort tests PASS, including 4 new wanted_games tests. `TestSelect_PinForcesNonClosestExpiry` no longer exists.

- [ ] **Step 5.5: Commit**

```bash
git add internal/farmer/dropselector.go internal/farmer/dropselector_test.go
git commit -m "farmer: selector sorts by wanted_games rank with empty-list fallback"
```

---

## Task 6: Daily-rolling completed-scrub

**Files:**
- Modify: `internal/farmer/drops.go`

- [ ] **Step 6.1: Add scrubStaleCompleted function**

In `internal/farmer/drops.go`, append at the end of the file (after the last existing function):

```go
// scrubStaleCompleted removes campaign IDs from CompletedCampaigns whose drops
// are again unclaimed in the new inventory. Twitch reuses the same campaign ID
// for daily-rolling campaigns (e.g. "Marble Day 245") and resets the drops —
// if we don't scrub, the bot silently skips them forever.
func (f *Farmer) scrubStaleCompleted(campaigns []twitch.DropCampaign) {
	for _, c := range campaigns {
		if !f.cfg.IsCampaignCompleted(c.ID) {
			continue
		}
		hasUnclaimed := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasUnclaimed = true
				break
			}
		}
		if hasUnclaimed {
			f.cfg.UnmarkCampaignCompleted(c.ID)
			f.addLog("[Drops] Campaign %q has unclaimed drops again — un-marking completed", c.Name)
		}
	}
	// Save once after all scrubs in this cycle (idempotent if no changes).
	f.cfg.Save()
}
```

- [ ] **Step 6.2: Call scrubStaleCompleted at start of processDrops**

In `internal/farmer/drops.go`, find `processDrops`. Locate the line right after the inventory fetch:

```go
f.writeLogFile(fmt.Sprintf("[Drops] Inventory returned %d campaigns", len(campaigns)))
```

Add immediately after that line:

```go
// Scrub stale CompletedCampaigns entries (daily-rolling reset detection)
f.scrubStaleCompleted(campaigns)
```

- [ ] **Step 6.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 6.4: Run existing test suite to ensure no regression**

Run: `go test ./...`

Expected: all PASS (no test for scrubStaleCompleted directly — it's a side-effect helper, smoke-tested in Task 19).

- [ ] **Step 6.5: Commit**

```bash
git add internal/farmer/drops.go
git commit -m "farmer: scrubStaleCompleted — recover daily-rolling campaigns"
```

---

## Task 7: Farmer event handler — `EventDropProgress` (local update)

**Files:**
- Modify: `internal/farmer/farmer.go`
- Modify: `internal/farmer/drops.go`

- [ ] **Step 7.1: Find the existing event-loop dispatch**

Run: `grep -n "func.*handleEvent\|case twitch.Event" internal/farmer/farmer.go | head -15`

Note the `handleEvent` function — it's a switch on `evt.Type`. New cases append at the end of the switch.

- [ ] **Step 7.2: Add EventDropProgress case**

In `internal/farmer/farmer.go`, find `handleEvent`. Locate the closing brace of its outer switch statement. Before the closing brace, add:

```go
case twitch.EventDropProgress:
	data := evt.Data.(twitch.DropProgressData)
	f.applyDropProgressUpdate(data)
```

- [ ] **Step 7.3: Add applyDropProgressUpdate method to drops.go**

In `internal/farmer/drops.go`, append at the end:

```go
// applyDropProgressUpdate handles a WebSocket drop-progress event by updating
// the in-memory ActiveDrops slice (so the web/TUI sees the new value within
// 1 second instead of waiting for the next 15-min poll). Also updates the
// matching channel's HasActiveDrop progress for TUI rendering.
func (f *Farmer) applyDropProgressUpdate(data twitch.DropProgressData) {
	f.drops.mu.Lock()
	updated := false
	for i := range f.drops.activeDrops {
		if f.drops.activeDrops[i].CampaignID != data.CampaignID {
			continue
		}
		if data.RequiredMinutesWatched > 0 && f.drops.activeDrops[i].Required != data.RequiredMinutesWatched {
			f.drops.activeDrops[i].Required = data.RequiredMinutesWatched
		}
		f.drops.activeDrops[i].Progress = data.CurrentMinutesWatched
		if f.drops.activeDrops[i].Required > 0 {
			pct := (data.CurrentMinutesWatched * 100) / f.drops.activeDrops[i].Required
			if pct > 100 {
				pct = 100
			}
			f.drops.activeDrops[i].Percent = pct
			f.drops.activeDrops[i].EtaMinutes = f.drops.activeDrops[i].Required - data.CurrentMinutesWatched
			if f.drops.activeDrops[i].EtaMinutes < 0 {
				f.drops.activeDrops[i].EtaMinutes = 0
			}
		}
		updated = true
		break
	}
	if updated && f.drops.lastPickCampaignID == data.CampaignID {
		f.drops.lastPickProgress = data.CurrentMinutesWatched
	}
	f.drops.mu.Unlock()

	if updated {
		f.writeLogFile(fmt.Sprintf("[Drops/WS] progress: campaign=%s drop=%s %d minutes",
			data.CampaignID, data.DropID, data.CurrentMinutesWatched))

		// Mirror to the channel's drop info so TUI shows the live value.
		f.drops.mu.RLock()
		pickedCh := f.drops.currentPickID
		f.drops.mu.RUnlock()
		if pickedCh != "" {
			f.mu.RLock()
			ch, ok := f.channels[pickedCh]
			f.mu.RUnlock()
			if ok {
				snap := ch.Snapshot()
				if snap.HasActiveDrop {
					ch.SetDropInfo(snap.DropName, data.CurrentMinutesWatched, snap.DropRequired)
				}
			}
		}
	}
}
```

- [ ] **Step 7.4: Verify build**

Run: `go build ./...`

Expected: exit 0. If `ChannelState` lacks `DropName`/`DropRequired` fields or `SetDropInfo` signature differs, run `grep "func.*SetDropInfo\|DropName\b\|DropRequired\b" internal/farmer/*.go | head` and adapt.

- [ ] **Step 7.5: Commit**

```bash
git add internal/farmer/farmer.go internal/farmer/drops.go
git commit -m "farmer: handle EventDropProgress — local activeDrops update from WebSocket"
```

---

## Task 8: Farmer event handler — `EventDropClaim`

**Files:**
- Modify: `internal/farmer/farmer.go`

- [ ] **Step 8.1: Add EventDropClaim case to handleEvent**

In `internal/farmer/farmer.go`, in the `handleEvent` switch, add after the `EventDropProgress` case:

```go
case twitch.EventDropClaim:
	data := evt.Data.(twitch.DropClaimData)
	if data.DropInstanceID != "" {
		instanceID := data.DropInstanceID
		go func() {
			if err := f.gql.ClaimDrop(instanceID); err != nil {
				f.addLog("[Drops/WS] Failed to claim drop: %v", err)
			} else {
				f.addLog("[Drops/WS] Claimed drop instance %s", instanceID)
			}
		}()
	}
	// Trigger an out-of-cycle inventory pull + selector run so that
	// (a) the next campaign in the queue gets a channel if this one finished,
	// (b) the campaign gets auto-marked completed if all drops are claimed.
	go f.processDrops()
```

- [ ] **Step 8.2: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 8.3: Commit**

```bash
git add internal/farmer/farmer.go
git commit -m "farmer: handle EventDropClaim — claim async + trigger out-of-cycle re-run"
```

---

## Task 9: Farmer event handler — `EventGameChange`

**Files:**
- Modify: `internal/farmer/farmer.go`

- [ ] **Step 9.1: Add EventGameChange case to handleEvent**

In `internal/farmer/farmer.go`, in `handleEvent` after the `EventDropClaim` case, add:

```go
case twitch.EventGameChange:
	data := evt.Data.(twitch.GameChangeData)
	f.handleChannelGameChange(evt.ChannelID, data)
```

- [ ] **Step 9.2: Add handleChannelGameChange method**

In `internal/farmer/farmer.go`, append at the end of the file (or near other channel-related helpers):

```go
// handleChannelGameChange reacts to a broadcast-settings-update PubSub event.
// If the channel was our current drop pick AND the new game does not match
// the picked campaign's game, the channel is added to stallCooldown for
// 15 min and an out-of-cycle selector re-run is triggered.
func (f *Farmer) handleChannelGameChange(channelID string, data twitch.GameChangeData) {
	if data.OldGameName == data.NewGameName {
		return // not actually a change
	}

	f.drops.mu.RLock()
	currentPick := f.drops.currentPickID
	pickCampaign := f.drops.lastPickCampaignID
	f.drops.mu.RUnlock()

	if channelID != currentPick {
		f.writeLogFile(fmt.Sprintf("[Drops/WS] non-pick channel %s game changed: %s -> %s",
			channelID, data.OldGameName, data.NewGameName))
		return
	}

	f.drops.mu.RLock()
	expectedGame := ""
	if c, ok := f.drops.campaignCache[pickCampaign]; ok {
		expectedGame = c.GameName
	}
	f.drops.mu.RUnlock()

	if expectedGame == "" {
		return
	}

	if strings.EqualFold(data.NewGameName, expectedGame) {
		f.addLog("[Drops/WS] %s switched back to %q — keeping pick", channelID, expectedGame)
		return
	}

	f.drops.mu.Lock()
	if f.drops.stallCooldown == nil {
		f.drops.stallCooldown = make(map[string]time.Time)
	}
	f.drops.stallCooldown[channelID] = time.Now().Add(15 * time.Minute)
	f.drops.mu.Unlock()

	f.addLog("[Drops/WS] %s changed game (%s -> %s); expected %s — 15min cooldown, re-picking",
		channelID, data.OldGameName, data.NewGameName, expectedGame)

	go f.processDrops()
}
```

- [ ] **Step 9.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 9.4: Commit**

```bash
git add internal/farmer/farmer.go
git commit -m "farmer: handle EventGameChange — instant game-mismatch detection"
```

---

## Task 10: Per-channel topic subscription delta in applySelectorPick

**Files:**
- Modify: `internal/farmer/drops.go`

- [ ] **Step 10.1: Add helpers to subscribe/unsubscribe broadcast-settings**

In `internal/farmer/drops.go`, append before `applySelectorPick`:

```go
// subscribeBroadcastSettings subscribes to the broadcast-settings-update topic
// for one channel so we get instant game-change notifications.
func (f *Farmer) subscribeBroadcastSettings(channelID string) {
	if f.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := f.pubsub.Listen([]string{topic}); err != nil {
		f.addLog("[PubSub] subscribe %s failed: %v", topic, err)
	}
}

// unsubscribeBroadcastSettings drops the broadcast-settings-update topic.
func (f *Farmer) unsubscribeBroadcastSettings(channelID string) {
	if f.pubsub == nil {
		return
	}
	topic := fmt.Sprintf("broadcast-settings-update.%s", channelID)
	if err := f.pubsub.Unlisten([]string{topic}); err != nil {
		f.addLog("[PubSub] unsubscribe %s failed: %v", topic, err)
	}
}
```

- [ ] **Step 10.2: Wire into applySelectorPick**

In `internal/farmer/drops.go`, find `applySelectorPick`. After the existing block:

```go
// Clear previous pick if it was a different channel.
if prevPickID != "" && prevPickID != pick.ChannelID {
    f.mu.RLock()
    prevCh, ok := f.channels[prevPickID]
    f.mu.RUnlock()
    if ok {
        prevCh.ClearDropInfo()
    }
}
```

Add immediately after that block (still inside `applySelectorPick`):

```go
// v1.8.0: per-channel WebSocket topics. Subscribe to the new pick's
// broadcast-settings-update; drop the previous pick's subscription if changed.
if prevPickID != "" && prevPickID != pick.ChannelID {
	f.unsubscribeBroadcastSettings(prevPickID)
}
f.subscribeBroadcastSettings(pick.ChannelID)
```

Also, in the `if pick == nil` branch (where we clear the previous pick on empty pool), add:

```go
if prevPickID != "" {
	f.unsubscribeBroadcastSettings(prevPickID)
}
```

- [ ] **Step 10.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 10.4: Commit**

```bash
git add internal/farmer/drops.go
git commit -m "farmer: subscribe/unsubscribe broadcast-settings per channel pick"
```

---

## Task 11: Subscribe `user-drop-events` at Farmer start, reduce poll cadence

**Files:**
- Modify: `internal/farmer/farmer.go`
- Modify: `internal/farmer/drops.go`

- [ ] **Step 11.1: Subscribe user-drop-events at startup**

In `internal/farmer/farmer.go`, find the existing PubSub user-level subscription:

```go
if err := f.pubsub.Listen([]string{
    fmt.Sprintf("community-points-user-v1.%s", user.ID),
}); err != nil {
    f.addLog("PubSub user topic error: %v", err)
}
```

Replace with:

```go
if err := f.pubsub.Listen([]string{
    fmt.Sprintf("community-points-user-v1.%s", user.ID),
    fmt.Sprintf("user-drop-events.%s", user.ID),
}); err != nil {
    f.addLog("PubSub user topic error: %v", err)
}
```

- [ ] **Step 11.2: Reduce polling cadence**

In `internal/farmer/drops.go`, find the `dropCheckLoop` function:

```go
ticker := time.NewTicker(5 * time.Minute)
```

Change to:

```go
ticker := time.NewTicker(15 * time.Minute) // v1.8.0: WebSocket carries the load; polling is safety net
```

- [ ] **Step 11.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 11.4: Commit**

```bash
git add internal/farmer/farmer.go internal/farmer/drops.go
git commit -m "farmer: subscribe user-drop-events at startup; polling 5min -> 15min"
```

---

## Task 12: TUI — `inputGameList` modal state + `g` keybind

**Files:**
- Modify: `internal/ui/app.go`

- [ ] **Step 12.1: Add new inputState constants + cursor state**

In `internal/ui/app.go`, find the inputState constants block. Add to the end:

```go
inputGameList    // v1.8.0: modal game-list editor
inputAddGameName // v1.8.0: prompt overlay inside the editor for new-game name
```

Find the Model struct (search `type Model struct`). Add this field:

```go
// v1.8.0 game-list editor state
gameListCursor int
```

- [ ] **Step 12.2: Add `g` keybind to handleKey normal-mode switch**

In `internal/ui/app.go`, find the `handleKey` function and the normal-mode switch (where `case "t":` etc. live). Add:

```go
case "g":
	m.inputMode = inputGameList
	m.gameListCursor = 0
	return m, nil
```

- [ ] **Step 12.3: Override key handling when in inputGameList mode**

Modify the existing top-of-function check in `handleKey`:

Find:
```go
if m.inputMode != inputNone {
    // ... existing text-input logic ...
}
```

Add BEFORE that block:

```go
if m.inputMode == inputGameList {
    return m.handleGameListKey(msg)
}
```

(So the order is: gameList check, then generic text-input check.)

- [ ] **Step 12.4: Add handleGameListKey method**

In `internal/ui/app.go`, append after `handleKey`:

```go
// handleGameListKey dispatches keys while the wanted-games modal editor is open.
func (m Model) handleGameListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	games := m.farmer.Config().GetGamesToWatch()

	switch msg.String() {
	case "esc", "enter":
		m.inputMode = inputNone
		_ = m.farmer.Config().Save()
		return m, nil

	case "up", "k":
		if m.gameListCursor > 0 {
			m.gameListCursor--
		}
		return m, nil

	case "down", "j":
		if m.gameListCursor < len(games)-1 {
			m.gameListCursor++
		}
		return m, nil

	case "u":
		if m.gameListCursor > 0 && m.gameListCursor < len(games) {
			m.farmer.Config().MoveGameToWatch(games[m.gameListCursor], -1)
			m.gameListCursor--
		}
		return m, nil

	case "d":
		if m.gameListCursor < len(games)-1 {
			m.farmer.Config().MoveGameToWatch(games[m.gameListCursor], +1)
			m.gameListCursor++
		}
		return m, nil

	case "-":
		if m.gameListCursor < len(games) {
			m.farmer.Config().RemoveGameFromWatch(games[m.gameListCursor])
			if m.gameListCursor > 0 && m.gameListCursor >= len(games)-1 {
				m.gameListCursor--
			}
		}
		return m, nil

	case "+":
		m.inputMode = inputAddGameName
		m.inputValue = ""
		return m, nil
	}
	return m, nil
}
```

- [ ] **Step 12.5: Add inputAddGameName submit handling**

In `internal/ui/app.go`, find the existing `submitInput` function. In its switch, add:

```go
case inputAddGameName:
	if value != "" {
		m.farmer.Config().AddGameToWatch(value)
		_ = m.farmer.Config().Save()
	}
	m.inputMode = inputGameList
	return m, nil
```

NOTE: this case must come BEFORE the unconditional `m.inputMode = inputNone` line at the bottom of `submitInput`. You'll need to restructure the bottom of the function so it doesn't override the inputGameList we just set:

Find the end of `submitInput`:

```go
m.inputMode = inputNone
return m, nil
```

Replace with:

```go
// inputAddGameName returns the user to the modal editor; everything else closes input.
if m.inputMode != inputGameList {
	m.inputMode = inputNone
}
return m, nil
```

- [ ] **Step 12.6: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 12.7: Commit**

```bash
git add internal/ui/app.go
git commit -m "ui: TUI inputGameList modal state + g keybind dispatch"
```

---

## Task 13: TUI — render the game-list editor modal

**Files:**
- Modify: `internal/ui/app.go` (renderInput method already has switch for prompts)
- Modify: `internal/ui/components.go` (add new renderGameListEditor)

- [ ] **Step 13.1: Add renderGameListEditor in components.go**

In `internal/ui/components.go`, append at the end:

```go
// renderGameListEditor draws the wanted-games modal overlay.
// Called when m.inputMode == inputGameList.
func renderGameListEditor(games []string, cursor int) string {
	header := titleStyle.Render(" Wanted Games (priority order) ")

	if len(games) == 0 {
		body := tableCellStyle.Render("  (empty — press '+' to add games)")
		footer := helpStyle.Render("  " +
			helpKeyStyle.Render("+") + helpStyle.Render(" add") +
			"  " + helpKeyStyle.Render("enter") + helpStyle.Render(" close") +
			"  " + helpKeyStyle.Render("esc") + helpStyle.Render(" close"))
		return header + "\n" + body + "\n" + footer
	}

	var rows []string
	for i, g := range games {
		marker := "  "
		if i == cursor {
			marker = "▸ "
		}
		row := fmt.Sprintf("%s%2d. %s", marker, i+1, g)
		if i == cursor {
			rows = append(rows, dropStyle.Render(row))
		} else {
			rows = append(rows, tableCellStyle.Render(row))
		}
	}

	keys := []struct{ key, desc string }{
		{"↑↓", "navigate"},
		{"+", "add"},
		{"-", "remove"},
		{"u", "up"},
		{"d", "down"},
		{"enter", "close"},
	}
	var helpParts []string
	for _, k := range keys {
		helpParts = append(helpParts, helpKeyStyle.Render(k.key)+helpStyle.Render(" "+k.desc))
	}
	footer := helpStyle.Render("  " + strings.Join(helpParts, "  |  "))

	return header + "\n" + strings.Join(rows, "\n") + "\n\n" + footer
}
```

- [ ] **Step 13.2: Update View to render the editor when in modal modes**

In `internal/ui/app.go`, find the `View` method. Locate the section that conditionally renders input prompts (search for `m.inputMode != inputNone`). After all other sections are appended to `sections` but BEFORE the bottom help bar, add:

```go
if m.inputMode == inputGameList || m.inputMode == inputAddGameName {
	games := m.farmer.Config().GetGamesToWatch()
	sections = append(sections, "", renderGameListEditor(games, m.gameListCursor))
}
```

- [ ] **Step 13.3: Add inputAddGameName case to renderInput**

In `internal/ui/app.go`, find `renderInput` method's switch. Add:

```go
case inputAddGameName:
	prompt = "Add game name: "
	hint = "  (Enter to confirm, Esc to cancel)"
```

- [ ] **Step 13.4: Update help bar — replace 'i' with 'g'**

In `internal/ui/components.go`, find `renderHelpBar`. Replace the existing pin keybind entry:

```go
{"i", "pin campaign"},
```

with:

```go
{"g", "wanted games"},
```

- [ ] **Step 13.5: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 13.6: Manual visual smoke test**

(Optional now — Task 19 covers full smoke test.)

- [ ] **Step 13.7: Commit**

```bash
git add internal/ui/app.go internal/ui/components.go
git commit -m "ui: TUI wanted-games modal editor (renderGameListEditor) + help bar"
```

---

## Task 14: TUI — remove `i` pin keybind (cleanup of v1.7.0)

**Files:**
- Modify: `internal/ui/app.go`

- [ ] **Step 14.1: Remove the `i` keybind handler**

In `internal/ui/app.go`, find:

```go
case "i":
	m.inputMode = inputPinCampaign
	m.inputValue = ""
	return m, nil
```

Delete those 4 lines.

- [ ] **Step 14.2: Remove inputPinCampaign submitInput case**

In `internal/ui/app.go`, find the `case inputPinCampaign:` block in `submitInput` (added in v1.7.0). Delete the entire case block.

- [ ] **Step 14.3: Remove inputPinCampaign from inputState constants**

Find the inputState constants block. Find and delete the `inputPinCampaign` line.

- [ ] **Step 14.4: Remove inputPinCampaign case from renderInput**

In `internal/ui/app.go`, find:

```go
case inputPinCampaign:
	prompt = "Pin campaign (partial name): "
	hint = "  (toggles pin; only one campaign can be pinned)"
```

Delete those lines.

- [ ] **Step 14.5: Verify build**

Run: `go build ./...`

Expected: exit 0. If anything else still references `inputPinCampaign`, grep and fix.

- [ ] **Step 14.6: Commit**

```bash
git add internal/ui/app.go
git commit -m "ui: remove TUI 'i' pin keybind — superseded by 'g' wanted-games"
```

---

## Task 15: Web API — `wanted_games` endpoints

**Files:**
- Modify: `internal/web/server.go`

- [ ] **Step 15.1: Add route registrations**

In `internal/web/server.go`, find `setupRoutes` function. Add:

```go
s.mux.HandleFunc("/api/wanted_games", s.handleWantedGames)
```

- [ ] **Step 15.2: Add handler**

In `internal/web/server.go`, append at the end of the file:

```go
func (s *Server) handleWantedGames(w http.ResponseWriter, r *http.Request) {
	cfg := s.farmer.Config()
	switch r.Method {
	case http.MethodGet:
		jsonResponse(w, map[string]interface{}{
			"games": cfg.GetGamesToWatch(),
		})
	case http.MethodPut:
		var req struct {
			Games []string `json:"games"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		cfg.SetGamesToWatch(req.Games)
		if err := cfg.Save(); err != nil {
			jsonError(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]interface{}{
			"games": cfg.GetGamesToWatch(),
		})
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
```

- [ ] **Step 15.3: Verify build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 15.4: Commit**

```bash
git add internal/web/server.go
git commit -m "web: GET/PUT /api/wanted_games endpoints"
```

---

## Task 16: Web UI — `Wanted Games` section + remove pin button

**Files:**
- Modify: `internal/web/static/index.html`

JS in this task uses safe DOM construction (createElement + textContent + replaceChildren) instead of template-string assignment to avoid XSS surface. The existing `escapeHtml` helper is still available for cases that need raw markup.

- [ ] **Step 16.1: Add Wanted Games section markup**

In `internal/web/static/index.html`, find the `<h2>Drop Campaigns</h2>` block. Add IMMEDIATELY BEFORE that block:

```html
<div class="section-header">
    <h2>Wanted Games</h2>
    <span class="section-help" style="font-size:0.85em;color:var(--text-muted)">priority list — top game wins</span>
</div>
<div class="table-container">
    <ul id="wanted-games-list" style="list-style:none;padding:0;margin:0">
        <li style="color:var(--text-muted);padding:8px">loading...</li>
    </ul>
    <div style="margin-top:8px;display:flex;gap:8px">
        <input id="wanted-games-input" type="text" placeholder="Game name (e.g. Arena Breakout: Infinite)"
               style="flex:1;padding:6px;background:var(--bg-secondary);border:1px solid var(--border);color:var(--text)">
        <button onclick="addWantedGame()" class="btn">Add</button>
    </div>
</div>
```

- [ ] **Step 16.2: Add JS using safe DOM construction**

In `internal/web/static/index.html`, find the existing `<script>` block. Append BEFORE the closing `</script>`:

```javascript
async function fetchWantedGames() {
    try {
        const res = await fetch(API + '/api/wanted_games');
        const data = await res.json();
        renderWantedGames(data.games || []);
    } catch (e) {
        console.error('Failed to fetch wanted games:', e);
    }
}

// Build DOM with createElement + textContent so user-supplied game names
// can never inject markup. No template-string assignment to .innerHTML here.
function renderWantedGames(games) {
    const ul = document.getElementById('wanted-games-list');
    while (ul.firstChild) ul.removeChild(ul.firstChild);

    if (!games || games.length === 0) {
        const li = document.createElement('li');
        li.style.color = 'var(--text-muted)';
        li.style.padding = '8px';
        li.textContent = 'empty — add a game below';
        ul.appendChild(li);
        return;
    }

    games.forEach((g, i) => {
        const li = document.createElement('li');
        li.dataset.idx = String(i);
        li.style.cssText = 'display:flex;align-items:center;padding:6px 8px;background:var(--bg-secondary);margin-bottom:4px;border:1px solid var(--border)';

        const idxSpan = document.createElement('span');
        idxSpan.style.cssText = 'width:24px;color:var(--text-muted)';
        idxSpan.textContent = (i + 1) + '.';
        li.appendChild(idxSpan);

        const nameSpan = document.createElement('span');
        nameSpan.style.flex = '1';
        nameSpan.textContent = g; // safe: textContent never parses markup
        li.appendChild(nameSpan);

        const upBtn = document.createElement('button');
        upBtn.className = 'btn-small';
        upBtn.textContent = '↑';
        upBtn.disabled = (i === 0);
        upBtn.onclick = () => moveWantedGame(i, -1);
        li.appendChild(upBtn);

        const downBtn = document.createElement('button');
        downBtn.className = 'btn-small';
        downBtn.textContent = '↓';
        downBtn.disabled = (i === games.length - 1);
        downBtn.onclick = () => moveWantedGame(i, +1);
        li.appendChild(downBtn);

        const removeBtn = document.createElement('button');
        removeBtn.className = 'btn-small';
        removeBtn.textContent = '✕';
        removeBtn.onclick = () => removeWantedGame(i);
        li.appendChild(removeBtn);

        ul.appendChild(li);
    });
}

function readCurrentWantedGames() {
    return Array.from(document.querySelectorAll('#wanted-games-list li[data-idx] > span:nth-child(2)'))
        .map(s => s.textContent);
}

async function putWantedGames(games) {
    try {
        const res = await fetch(API + '/api/wanted_games', {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({games})
        });
        const data = await res.json();
        renderWantedGames(data.games || []);
    } catch (e) {
        showToast('Failed to update wanted games: ' + e.message, 'error');
    }
}

async function addWantedGame() {
    const input = document.getElementById('wanted-games-input');
    const name = input.value.trim();
    if (!name) return;
    const current = readCurrentWantedGames();
    current.push(name);
    input.value = '';
    await putWantedGames(current);
}

async function removeWantedGame(idx) {
    const current = readCurrentWantedGames();
    current.splice(idx, 1);
    await putWantedGames(current);
}

async function moveWantedGame(idx, dir) {
    const current = readCurrentWantedGames();
    const target = idx + dir;
    if (target < 0 || target >= current.length) return;
    [current[idx], current[target]] = [current[target], current[idx]];
    await putWantedGames(current);
}
```

Also find the existing periodic refresh (search for `setInterval`). Add a call to `fetchWantedGames` in the same interval handler. And find page-load init — add `fetchWantedGames();` near where other initial fetches are made.

- [ ] **Step 16.3: Remove pin button column from drops table**

In `internal/web/static/index.html`, find the drops table `<thead>` (added in v1.7.0). Delete the line:

```html
<th>📌</th>
```

In the drops-render JavaScript (search for `pin-btn` or `togglePin`), remove the pin column production. The existing pin button cell looks like:

```javascript
const pinBtn = isCompleted ? '' :
    `<button class="pin-btn" ...></button>`;
```

Delete that variable definition and any `${pinBtn}` cell reference. Also delete the `<td>${drop.is_pinned ? '📌' : ''}</td>` cell if present.

The `togglePin` function and `.pin-btn` CSS can stay (orphaned but harmless), or be removed for tidiness.

- [ ] **Step 16.4: Verify build**

Run: `go build ./...`

Expected: exit 0 (HTML changes don't affect Go compilation, just confirms embed still works).

- [ ] **Step 16.5: Commit**

```bash
git add internal/web/static/index.html
git commit -m "web: Wanted Games section with reorder UI; remove pin column"
```

---

## Task 17: Version bump + changelog + build-all

**Files:**
- Modify: `cmd/twitchpoint/main.go`
- Modify: `changelog.txt`

- [ ] **Step 17.1: Bump version**

In `cmd/twitchpoint/main.go`, change:

```go
const appVersion = "1.7.0"
```

to:

```go
const appVersion = "1.8.0"
```

- [ ] **Step 17.2: Add changelog entry**

Get timestamp: `date "+%Y-%m-%d %H:%M"`.

Prepend to `changelog.txt` (top of file, before existing v1.7.0 entry):

```
== <YYYY-MM-DD HH:MM> == Major: WebSocket-driven drops + wanted_games priority (v1.8.0) ==

Replaces 5-min inventory polling with Twitch PubSub WebSocket for
real-time drop progress + game-change detection, and replaces the v1.7.0
remaining_time + pin sort with a user-ordered wanted_games priority list.
Inspired by TwitchDropsMiner.

What changed:
- WebSocket: subscribe user-drop-events.<userID> at startup. drop-progress
  events update activeDrops in-memory (UI shows new progress within ~1s
  instead of waiting 5-15 min for next poll). drop-claim events trigger
  ClaimDrop async + an out-of-cycle inventory refresh.
- WebSocket: subscribe broadcast-settings-update.<channelID> for the
  currently picked channel. Game-change events trigger a 15-min cooldown
  for that channel and immediate re-pick — fixes the case where a
  streamer switches mid-stream from ABI to Just Chatting and we keep
  watching the wrong game.
- New PubSubClient.Unlisten method to drop per-channel subscriptions
  when the selector switches to a different channel.
- New config field GamesToWatch (ordered list of game names) with helpers
  Add/Remove/Move/Set/GetGamesToWatch.
- Selector sortPool now uses (wanted_games_rank, earliest_endAt,
  viewer_count) instead of (pinned, endAt, viewers). When a channel
  serves multiple campaigns from different games, the BEST (lowest)
  game-rank wins. Empty wanted_games falls back to v1.7.0
  remaining_time-only sort — fully backward compatible.
- TUI: new modal game-list editor on 'g' keybind. ↑↓ navigate, + add
  (text-input prompt), - remove, u/d move up/down, enter/esc close
  (autosaves on close). 'i' pin keybind removed.
- Web UI: new Wanted Games section above Drop Campaigns with
  add/up/down/remove plain-JS controls (safe DOM construction). Pin
  column removed from drops table.
- New scrubStaleCompleted: at the start of each cycle, remove campaign
  IDs from CompletedCampaigns whose drops are again unclaimed in the new
  inventory. Fixes the v1.7.0 bug where Marble Day 245 (daily-rolling
  campaign) got marked completed forever.
- Polling cadence reduced from 5 min to 15 min (WebSocket carries the
  real-time load; polling is safety net for missed events / WS disconnects).

What we KEEP from v1.7.0 (TDM doesn't have these and we are ahead):
- Channel-pool architecture
- Stall-cooldown (demote-on-no-credit, 30 min)
- Benefit-ID dedup via pool

Backward compat:
- PinnedCampaignID field stays in Config schema (silently ignored).
- Empty GamesToWatch behaves exactly like v1.7.0 — opt-in to new sort
  by adding games.

Files: internal/config/config.go, internal/config/config_test.go,
internal/twitch/types.go, internal/twitch/pubsub.go,
internal/farmer/farmer.go, internal/farmer/drops.go,
internal/farmer/dropselector.go, internal/farmer/dropselector_test.go,
internal/ui/app.go, internal/ui/components.go,
internal/web/server.go, internal/web/static/index.html,
cmd/twitchpoint/main.go (1.7.0 -> 1.8.0).
```

(Replace `<YYYY-MM-DD HH:MM>` with the actual `date` output.)

- [ ] **Step 17.3: Run full test suite**

Run: `go test ./...`

Expected: all PASS.

- [ ] **Step 17.4: Build all platform binaries**

Run: `make build-all`

Expected:
```
GOOS=darwin  GOARCH=arm64 go build -o bin/twitchpoint-macos       ./cmd/twitchpoint
GOOS=linux   GOARCH=amd64 go build -o bin/twitchpoint-linux       ./cmd/twitchpoint
GOOS=windows GOARCH=amd64 go build -o bin/twitchpoint-windows.exe ./cmd/twitchpoint
```

- [ ] **Step 17.5: Commit**

```bash
git add cmd/twitchpoint/main.go changelog.txt bin/twitchpoint-macos bin/twitchpoint-linux bin/twitchpoint-windows.exe
git commit -m "release: v1.8.0 — WebSocket drops + wanted_games priority"
```

---

## Task 18: Push + tag + GitHub release

**Files:** none (release operations only)

- [ ] **Step 18.1: Push commits**

Run: `git push origin main`

Expected: commits pushed.

- [ ] **Step 18.2: Tag + push tag**

Run:

```bash
git tag -a v1.8.0 -m "v1.8.0 — WebSocket drops + wanted_games priority"
git push origin v1.8.0
```

Expected: tag pushed.

- [ ] **Step 18.3: Create GitHub Release with binaries**

Run:

```bash
gh release create v1.8.0 \
  bin/twitchpoint-linux \
  bin/twitchpoint-macos \
  bin/twitchpoint-windows.exe \
  --title "v1.8.0 — WebSocket drops + wanted_games priority" \
  --notes "$(cat <<'EOF'
## Major: WebSocket-driven drops + user-ordered game priority

Replaces 5-min inventory polling with Twitch's real-time PubSub WebSocket and gives you a `wanted_games` ordered list to control which games the bot prioritizes. Inspired by [TwitchDropsMiner](https://github.com/rangermix/TwitchDropsMiner).

### What's new

- **WebSocket integration** — drop progress and claim events fire within ~1 second. Game-change detection per watched channel via `broadcast-settings-update`. No more 5-15 min lag.
- **`wanted_games` priority list** — order the games you care about; selector picks channels covering your top game first. Empty list = v1.7.0 remaining-time behavior (backward compat).
- **TUI modal game-list editor** — press `g`, add/remove/reorder games right in the terminal. No web UI required.
- **Web UI** — new Wanted Games section with add/up/down/remove controls. Pin button removed (superseded by game ordering).
- **Daily-rolling campaign fix** — campaigns like Marble Day that Twitch resets daily now get auto-uncompleted when their drops show unclaimed in fresh inventory.
- **Polling reduced 5min → 15min** — WebSocket carries the real-time load; polling is just a safety net.

### What stays from v1.7.0 (we are ahead of TDM here)

- Channel-pool architecture
- Stall-cooldown (demote-on-no-credit, 30 min)
- Benefit-ID dedup via pool

### Compatibility

- Backward compatible: existing v1.7.0 configs load and behave the same until you add games to `wanted_games`
- `PinnedCampaignID` config field kept for forward-compat (silently ignored)
- Channel-points farming unaffected
EOF
)"
```

Expected: release URL printed.

- [ ] **Step 18.4: Verify release**

Run: `gh release view v1.8.0 --json tagName,assets | jq '{tag: .tagName, assets: [.assets[].name]}'`

Expected: tag `v1.8.0` and 3 asset names.

---

## Task 19: Manual smoke test against running bot

**Files:** none (verification only)

- [ ] **Step 19.1: Stop running bot, start v1.8.0**

```bash
ps aux | grep twitchpoint | grep -v grep
kill <PID>
./bin/twitchpoint-macos &
```

Wait ~30s for first cycle.

- [ ] **Step 19.2: Verify WebSocket subscribed**

```bash
grep -E "subscribed.*user-drop-events|connected, subscribed" logs/debug-$(date +%Y-%m-%d).log | tail -3
```

Expected: at least one line confirming the subscription.

- [ ] **Step 19.3: Verify WebSocket fires on drop progress**

```bash
tail -f logs/debug-$(date +%Y-%m-%d).log | grep "Drops/WS"
```

Expected within ~1 minute of watching: `[Drops/WS] progress: campaign=... drop=... N minutes` (only if Twitch is actively crediting).

- [ ] **Step 19.4: Test wanted_games via TUI**

In TUI: press `g` → editor opens. Press `+` → prompt. Type "arena breakout: infinite" → Enter. Editor shows the game. Press `enter` to close.

```bash
jq '.games_to_watch' config.json
```

Expected: `["arena breakout: infinite"]`.

- [ ] **Step 19.5: Test wanted_games via Web**

Open `http://localhost:<port>/`. Verify "Wanted Games" section visible above "Drop Campaigns". Type a game name → click Add → row appears.

- [ ] **Step 19.6: Test pin field backward compat**

```bash
jq '.pinned_campaign_id' config.json
```

Expected: bot starts cleanly, no errors about pin.

- [ ] **Step 19.7: Test daily-rolling scrub**

```bash
grep "un-marking completed" logs/debug-$(date +%Y-%m-%d).log | tail -3
```

Expected: log line if any campaign was scrubbed.

- [ ] **Step 19.8: Test channel-points farming still works**

```bash
grep "+10 points" logs/debug-$(date +%Y-%m-%d).log | tail -5
```

Expected: regular WATCH credits.

- [ ] **Step 19.9: Test game-change detection**

When a watched streamer changes game:

```bash
grep "Drops/WS.*changed game\|Drops/WS.*switched back" logs/debug-$(date +%Y-%m-%d).log | tail -3
```

Expected: log line within seconds of streamer's category change.

---

## Self-Review

**Spec coverage:**
- ✅ WebSocket user-drop-events → Tasks 4, 11, 7, 8
- ✅ WebSocket broadcast-settings-update → Tasks 4, 9, 10
- ✅ wanted_games config + helpers → Task 1
- ✅ Selector sort change → Task 5
- ✅ TUI modal editor → Tasks 12, 13, 14
- ✅ Web /api/wanted_games + UI section → Tasks 15, 16
- ✅ Daily-rolling completed-scrub → Task 6
- ✅ Polling cadence reduction → Task 11
- ✅ Pin field backward compat (silent ignore) → Tasks 5 + 14
- ✅ Version bump + changelog + build → Task 17
- ✅ Push + tag + GH release → Task 18
- ✅ Smoke test → Task 19

**Placeholder scan:** none. The only `<placeholder>` is `<YYYY-MM-DD HH:MM>` in Task 17.2 changelog (instruction is to replace via `date` command).

**Type/method consistency:**
- `GamesToWatch`, `GetGamesToWatch`, `AddGameToWatch`, `RemoveGameFromWatch`, `MoveGameToWatch`, `SetGamesToWatch`, `UnmarkCampaignCompleted` — defined in Task 1, used in Tasks 5/12/15.
- `EventDropProgress`, `EventDropClaim`, `EventGameChange` — defined in Task 2, used in Tasks 4/7/8/9.
- `DropProgressData`, `DropClaimData`, `GameChangeData` — defined in Task 2, used in Tasks 4/7/8/9.
- `subscribeBroadcastSettings`, `unsubscribeBroadcastSettings`, `applyDropProgressUpdate`, `scrubStaleCompleted`, `handleChannelGameChange` — all defined where they're added.
- `inputGameList`, `inputAddGameName`, `gameListCursor`, `handleGameListKey`, `renderGameListEditor` — defined in Tasks 12/13.
- `Unlisten` method on PubSubClient — defined in Task 3, used in Task 10.
- Topic strings: `user-drop-events.<userID>` (Task 11), `broadcast-settings-update.<channelID>` (Task 10) consistent with handler prefixes in Task 4.

**Note for executor:** if grep-anchors fail (e.g. line numbers shifted), use the surrounding-code anchors (e.g. "find the existing PubSub user-level subscription") and search dynamically rather than trusting absolute line numbers.
