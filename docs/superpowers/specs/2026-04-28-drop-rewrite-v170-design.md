# v1.7.0 — Drop-Subsystem Rewrite (Channel-Pool Selector)

**Date:** 2026-04-28
**Author:** miwi + Claude
**Status:** Approved (brainstorming complete, awaiting plan)
**Replaces:** drop-related code in v1.6.5

## Why

The current parallel multi-drop architecture has produced a chain of bugs that
fix one symptom and expose the next:

- **v1.5.2 / v1.5.3:** exclusive drops blocking, benefit-ID dedup
- **v1.6.0 / v1.6.1:** smart exclusive dedup, disabled-campaign fix
- **v1.6.2:** dead temp channels, drops-enabled stream picks (Bug A only)
- **v1.6.3:** failover did not transfer drop state (transferDrop helper)
- **v1.6.4:** drops-enabled filter for Strategy 1 + stall-failover (Bug B)
- **v1.6.5:** stall-failover picked the failing channel as its own replacement

The latest live test (2026-04-28) showed:
- Bot stuck on `samoylov___` for 3+ hours with 0 progress
- After fix, picked `buggy` who is genuinely drops-enabled per Twitch's API but
  Twitch still credited 0 minutes
- Cascading state corruption: drop pointed at a channel that no longer existed
  in `f.channels`, Spade slots taken by `axel_tv` (Tarkov) and `sirhansvader`
  (Just Chatting) instead of the drop channel

Root cause across all of these is the same: **multiple parallel state machines**
(per-campaign drop state, per-channel state, per-Spade-slot state) that must
stay in sync but don't. Every patch added another sync rule. Time to collapse
the model.

The reference implementation
[TwitchDropsMiner](https://github.com/rangermix/TwitchDropsMiner) uses a much
simpler model: build a **channel pool** from all eligible campaigns, sort,
pick one. The user agreed this is "way smoother and less error-prone."

## What

A single channel-pool selector that replaces ~500 lines of campaign-iteration,
failover, and stall-detection logic. Pin support gives the user a one-click
override of the auto-selected priority.

### Algorithm (cycle-by-cycle, every 5 min)

```
1. Pull inventory (existing GQL: ViewerDropsDashboard + Inventory merge)
2. Filter eligible campaigns:
   - status == "ACTIVE"
   - IsAccountConnected == true
   - !IsCampaignDisabled
   - !IsCampaignCompleted
   - endAt > now
   - has at least one drop with RequiredMinutesWatched > 0 and !claimed
3. Build channel pool:
   For each eligible campaign sorted by (pinned desc, remaining_time asc):
     - If campaign has allowed channels: add those that are live + drops-enabled
       (cross-reference allowed list against drops-enabled directory)
     - If unrestricted: take top N from drops-enabled game directory
   Each pool entry remembers WHICH campaigns it serves.
4. Dedupe pool: each channel appears once. If a channel serves multiple
   campaigns, it inherits the highest-priority campaign for sort purposes
   but the bot will progress all matching campaigns when watching it
   (Twitch credits whichever can_earn).
5. Sort pool by sortKey: (pinned-first, earliest-end-time asc, viewer-count desc)
6. Pick pool[0] → that's the channel for Spade slot 1
7. Spade slot 2 → existing P1/P2 channel-points rotation (unchanged)
8. If pool empty (no live drops-enabled channel for any eligible campaign) →
   both Spade slots go to P1/P2; bot is "drop-idle" but still farms points
9. After 5 min, repeat from step 1. Channel switching is the atomic operation.
```

### Why this is simpler than v1.6.x

| Concern | v1.6.x approach | v1.7.0 approach |
|---|---|---|
| Multiple campaigns per cycle | Iterate, assign per-campaign | Single channel pick |
| Same channel serves N campaigns | Benefit-ID dedup logic | Pool dedup, free |
| Stream goes offline mid-watch | Stall-detection + failover state | Next cycle, next pick |
| Wrong channel picked | Failover with cooldown + retry | Next cycle, next pick |
| Spade slot saturation | Skip campaigns with checks | Pool already filtered to top |

Everything that was a special case becomes "next cycle, next pick."

## Architecture

### New file: `internal/farmer/dropselector.go` (~250 LoC)

```go
type DropSelector struct {
    cfg    *config.Config
    gql    *twitch.GQLClient
    log    func(format string, args ...interface{})  // injected logger
}

type PoolEntry struct {
    ChannelID    string
    ChannelLogin string
    DisplayName  string
    BroadcastID  string
    ViewerCount  int
    Campaigns    []CampaignRef     // 1+ campaigns this channel serves
}

type CampaignRef struct {
    ID            string
    Name          string
    GameName      string
    EndAt         time.Time
    IsPinned      bool
}

// SelectChannel returns the single best channel to watch right now,
// or empty (channelID, "") if no eligible drops-enabled channel exists.
// Also returns the full sorted queue for UI display.
func (s *DropSelector) SelectChannel(campaigns []twitch.DropCampaign) (
    pick *PoolEntry,
    queue []*PoolEntry,
)
```

### Rewritten file: `internal/farmer/drops.go` (~400 LoC, was 958)

Slim coordinator:
- `dropCheckLoop()` — same polling loop as today (5 min cycle)
- `processDrops()` — calls selector, applies result to Spade slot 1, claims completed drops
- `ActiveDrop` web-API struct — augmented with `Status`, `IsPinned` fields
- `GetActiveDrops()` and `GetCampaignQueue()` — for web UI consumption

Removed:
- `findAllowedChannelViaDirectory`
- `findLiveFromAllowedChannels`
- `autoSelectDropChannel`
- `pickExclusiveCampaigns`
- `checkDropProgressStalls`
- `cleanupFailoverCooldowns`
- `dropStallCount`, `failoverCooldowns` fields
- `spadeSlotsSaturated`

### Modified file: `internal/farmer/farmer.go`

- **Removed:** `handleDropFailover`, `transferDrop` (closure inside it),
  `verifyTempChannelHealth` (the stale-detection part — replaced by
  the natural cycle re-pick), `findLiveFromGameDirectory`,
  `findLiveFromGameDirectoryExcluding`
- **Modified:** `rotateChannels` reserves Spade slot 1 for the selector's
  current pick if any. Slot 2 follows existing P1 → P2 rotation.
- **Kept:** `addTemporaryChannel` (still used by selector to register newly
  picked channels), `removeTemporaryChannel`, channel-points machinery, IRC,
  PubSub, points-claim handling.

### Config additions: `internal/config/config.go`

```go
type Config struct {
    // ... existing fields ...
    PinnedCampaignID string `json:"pinned_campaign_id,omitempty"`
}

func (c *Config) SetPinnedCampaign(id string)        // empty string = no pin
func (c *Config) IsCampaignPinned(id string) bool
```

Backward compatible — old configs load with empty pin.

### Web API: `internal/web/server.go`

Existing `GET /api/drops` response struct gains:
```go
type ActiveDrop struct {
    // ... existing fields ...
    Status     string `json:"status"`     // ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED
    IsPinned   bool   `json:"is_pinned"`
    QueueIndex int    `json:"queue_index"` // 1-based for ACTIVE/QUEUED/IDLE, 0 otherwise
    EtaMinutes int    `json:"eta_minutes"` // RequiredMinutesWatched - CurrentMinutesWatched of the next-to-claim drop in this campaign (no rate prediction — pure remaining minutes)
}
```

New endpoint:
```
PUT /api/drops/{id}/pin    body: {"pinned": true} or {"pinned": false}
```
- `{"pinned": true}` on campaign X: sets `Config.PinnedCampaignID = X` (clearing any previous pin atomically — only one pin at a time)
- `{"pinned": false}` on the currently-pinned campaign: clears the pin
- `{"pinned": false}` on a not-pinned campaign: no-op, returns 200
- Returns the new state: `{"pinned_campaign_id": "X" or ""}`

### Web UI: `internal/web/server.go` (embedded HTML)

New columns: `📌` (pin button), `#` (queue index), `Status`, `ETA`.
Sort order: PINNED → ACTIVE → QUEUED (by remaining_time) → IDLE → DISABLED → COMPLETED.
Auto-refresh stays at 5s.

## Data flow

```
                   ┌──────────────────┐
                   │  dropCheckLoop   │
                   │  (every 5 min)   │
                   └────────┬─────────┘
                            │
                ┌───────────▼────────────┐
                │  GetDropsInventory()   │   GQL Dashboard + Inventory
                └───────────┬────────────┘
                            │
                ┌───────────▼────────────┐
                │  Selector.Select(...)  │   Build pool, sort, pick
                └───────────┬────────────┘
                            │
              ┌─────────────┼─────────────────┐
              │             │                 │
              ▼             ▼                 ▼
      ┌────────────┐  ┌────────────┐   ┌─────────────┐
      │ Slot 1 set │  │ Cache new  │   │ Auto-claim  │
      │ to picked  │  │ activeDrop │   │ completed   │
      │ channel    │  │ for web UI │   │ drops       │
      └─────┬──────┘  └────────────┘   └─────────────┘
            │
            ▼
   ┌──────────────────┐
   │ rotateChannels() │   Slot 1 reserved, slot 2 = P1/P2
   └──────────────────┘
```

## Migration & cutover

**Config:** zero migration needed. New field optional, old fields untouched.

**Cutover steps (this order):**
1. Add `PinnedCampaignID` to Config struct + helpers
2. Write `dropselector.go` (new file, no conflicts)
3. Rewrite `drops.go` against the new selector
4. Strip removed functions from `farmer.go`, adjust `rotateChannels`
5. Add Pin endpoint + extend `/api/drops` response in `web/server.go`
6. Update embedded HTML in `web/server.go` (drops table layout)
7. `go build ./...` + `go vet ./...` clean
8. Manual smoke test against running Twitch account
9. Version bump 1.6.5 → 1.7.0
10. Changelog entry
11. `make build-all`
12. Commit + restart bot

**Rollback:** keep `bin/twitchpoint-macos.bak` (and equivalents) as v1.6.5
copy before restart. Config is forward-compatible (new field optional).

## Test plan

| Scenario | Expected |
|---|---|
| Bot start, ABI Partner-Only ACTIVE, drops-enabled streamers live | Selector picks one, Spade slot 1 = that streamer |
| Pin Marvel Rivals while ABI is current | Switch to Marvel within 1 cycle |
| ABI completes (all drops claimed) | Auto-marks completed, queue advances to next |
| Disable all eligible campaigns | Both Spade slots go to P1 always-watch / P2 rotate |
| Picked streamer goes offline mid-cycle | Next cycle picks a replacement, no special handling |
| Two campaigns share the same allowed channel | Channel picked once, both campaigns progress |
| Twitch returns 9 duplicate "S5 Support ABI Partners" | Pool dedup makes it irrelevant — channel picked once |
| Empty pool (no live drops-enabled channel anywhere) | Bot logs "drop-idle", uses both slots for points |
| Restart preserves Pin from previous session | `pinned_campaign_id` reloaded from config |

## Out of scope for v1.7.0

- TUI pin keybind (planned v1.7.1)
- Drop-completion notifications (Discord/Browser)
- Per-drop ETA prediction (basic ETA = remaining_minutes only)
- Persistent farming statistics
- Game-priority list (TwitchDropsMiner-style) — campaign-level pin is enough

## Acceptance criteria

1. `go build ./...` and `go vet ./...` exit 0
2. After bot restart on v1.7.0:
   - Drop is assigned to a drops-enabled channel within 1 inventory cycle
   - Progress increments visibly within 2 cycles (or another channel is picked)
3. Pin toggle in web UI causes immediate channel switch on next cycle
4. Channel-points farming on slot 2 is unaffected (P1 always-watch still works)
5. No `[Drops/Health]`, `[Drops/Failover]`, `[Drops/AutoSelect Strategy 1/2]`,
   or `Reusing already-tracked` log spam — replaced by:
   - `[Drops/Pool] built pool of N channels for M eligible campaigns`
   - `[Drops/Pool] picked X (campaigns: a, b, c)`
   - `[Drops/Pool] empty pool — drops idle, slots free for points`

## Files touched (estimate)

| File | Lines today | Lines after | Delta |
|---|---|---|---|
| internal/farmer/drops.go | 958 | ~400 | -558 |
| internal/farmer/farmer.go | 1219 | ~1000 | -219 |
| internal/farmer/dropselector.go | 0 | ~250 | +250 |
| internal/config/config.go | 305 | ~320 | +15 |
| internal/web/server.go | 335 | ~395 | +60 |
| cmd/twitchpoint/main.go | 1 line const | 1 line const | 0 (version bump only) |
| changelog.txt | append | append | +1 entry |

Net code reduction: ~450 lines removed.
