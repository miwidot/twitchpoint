# v1.8.0 — WebSocket-driven drops + `wanted_games` priority

**Date:** 2026-04-29
**Author:** miwi + Claude
**Status:** Approved (brainstorming complete, awaiting plan)
**Replaces / extends:** v1.7.0 channel-pool selector

## Why

After v1.7.0 shipped, the live experience surfaced three things:

1. **5-min polling is too slow.** When buggy stalled (Twitch refusing to credit), the bot took 10 min (2 cycles) to detect and switch. When a streamer changes game category mid-stream (ABI → Just Chatting), the bot keeps watching the wrong game until the next 5-min cycle. The drop UI shows stale progress for up to 5 min.

2. **`remaining_time` sort doesn't always match user intent.** If the user has 3 active games (Arena Breakout, Marvel Rivals, Honkai), the bot picks whatever expires soonest — but the user might prefer "always farm Arena Breakout first when there's a live channel, then everything else." The `pin` field allows pinning ONE campaign but not "this game over those games."

3. **Daily-rolling campaigns get stuck in `completed_campaigns`.** "Marble Day 245" was correctly marked completed once, but Twitch resets the campaign daily without changing the ID — bot silently skips it forever.

The reference implementation
[TwitchDropsMiner](https://github.com/rangermix/TwitchDropsMiner) handles #1
and #2 cleanly via Twitch's PubSub WebSocket and a user-ordered
`games_to_watch` list. Research report: TDM uses `wss://pubsub-edge.twitch.tv/v1`
with `user-drop-events.<userID>`, `broadcast-settings-update.<channelID>`,
and `video-playback-by-id.<channelID>` topics. We already have the latter
for stream-up/down detection — we extend the existing PubSub plumbing.

**v1.7.0 wins we KEEP** (TDM doesn't have these and we are ahead):
- Channel-pool architecture
- Drop progress stall-cooldown (`stallCooldown` map, demote-on-no-credit)
- Benefit-ID dedup via pool

## What

### 1. WebSocket-driven drop progress (Topic A: `user-drop-events`)

Subscribe at startup to `user-drop-events.<userID>`. Two relevant message types:

- `drop-progress` — payload includes `current_progress_min`, `drop_id`. **Reaction:** update local `f.drops.activeDrops[i].Progress` for the matching drop. UI sees instant updates, no GQL call needed.
- `drop-claim` — payload includes `drop_instance_id`. **Reaction:** call `gql.ClaimDrop()` in goroutine, then trigger an out-of-cycle inventory refresh + selector run (covers "all drops claimed → mark campaign completed → pick next campaign").

This collapses the gap between "Twitch credited a minute" and "we know about it" from up to 5 min to under a second.

### 2. Per-channel WebSocket subscriptions (Topics B + C improvements)

When the selector picks a channel and `applySelectorPick` registers it as a temp channel, we also subscribe to:

- `broadcast-settings-update.<channelID>` — fires when streamer changes game/title. **Reaction:** if new game ≠ campaign's expected game, mark the channel as game-mismatched in `stallCooldown` (15 min, shorter than no-credit cooldown) and trigger out-of-cycle selector re-run.
- `video-playback-by-id.<channelID>` — we already subscribe; behavior unchanged. The `stream-down` event already exists in our event handler. **Implementation:** when the picked drop channel goes offline (`EventStreamDown` fires AND channelID == currentPickID), trigger an out-of-cycle `processDrops` so the selector picks a new drops-enabled channel within seconds instead of waiting up to 15 minutes for the next inventory cycle. v1.7.0 removed this immediate re-run; v1.8.0 must re-introduce it for the picked channel only (keep the v1.7.0 silence for non-pick channels to avoid flooding processDrops).

When the selector picks a different channel next cycle, the previous channel's per-channel topics are unsubscribed. PubSub topic delta is computed in `applySelectorPick`.

### 3. `games_to_watch` ordered list (replaces `remaining_time` primary sort)

New config field:
```go
GamesToWatch []string `json:"games_to_watch,omitempty"` // ordered, lowercase game names
```

Helpers on `*Config`:
- `GetGamesToWatch() []string` — returns ordered slice
- `AddGameToWatch(game string)` — appends if not present
- `RemoveGameFromWatch(game string)` — removes by case-insensitive match
- `MoveGameToWatch(game string, direction int)` — direction: -1 up, +1 down
- `IsGameWanted(game string) (rank int, ok bool)` — returns 0-based index, false if not in list

**Selector sort change** (`internal/farmer/dropselector.go:sortPool`):

```
Sort key per PoolEntry (priority order):
  1. game rank in wanted_games (lower index = higher priority)
     - if game NOT in wanted_games: rank = len(wanted_games) (sorts after all wanted games)
     - special case: if wanted_games is EMPTY → fallback to remaining_time (current v1.7.0 behavior)
  2. closest endAt within same game-rank tier
  3. viewer_count desc as tie-break
```

Empty `wanted_games` = backward-compatible: bot behaves as v1.7.0.

**Pin field handling:** `PinnedCampaignID` stays in Config struct for backward
compat, silently ignored by selector. Web UI pin button removed. TUI `i`
keybind removed (replaced by `g` for game-list editor). Existing pinned
campaigns just don't get pinned anymore — no error, no migration code.

### 4. TUI modal editor for `wanted_games` (`g` keybind)

New modal sub-screen (not a prompt — full overlay):

```
┌─ Wanted Games (priority order) ─────────────────────┐
│  1. Arena Breakout: Infinite                  ←★    │
│  2. Marvel Rivals                                   │
│  3. World of Tanks                                  │
│  4. Honkai: Star Rail                               │
│                                                     │
│  ↑↓ navigate | + add | - remove | u up | d down     │
│  enter close | esc close                            │
└─────────────────────────────────────────────────────┘
```

- `g` opens; `↑/↓` navigate cursor row; `u`/`d` move highlighted item up/down.
- `+` opens text-input prompt "Add game name:" — fuzzy autocomplete against eligible inventory campaigns.
- `-` removes highlighted item.
- `enter` or `esc` closes editor. Config saved on close (single `Save()` call).

New `inputState` value `inputGameList`. New struct field `gameListCursor int`.

The drops table on the main screen does NOT change layout — it just sorts by the new key.

### 5. Web UI updates

- Pin button removed from drops table.
- New collapsible section "Wanted Games" with drag-and-drop list (sortable.js or similar minimal lib — no React, plain JS) plus add/remove buttons.
- `GET /api/wanted_games` → returns the ordered list
- `PUT /api/wanted_games` → body: `{"games": ["a", "b", "c"]}` — replaces the whole list atomically

### 6. Source-of-truth: `dropCampaignsInProgress`

**Key insight from live testing:** Twitch's dashboard query (`dropCampaigns`)
cannot be trusted to report claim status truthfully. Marble Day 245 showed
`isClaimed: false` for all 6 drops in dashboard, but `DropCurrentSession`
reported `currentMinutesWatched: 744 / requiredMinutesWatched: 720` —
the user had already claimed everything externally.

The reliable signal is `InInventory` (`dropCampaignsInProgress`):
- **In progress AND drops have current < required** → still farmable
- **Not in progress** (only in dashboard) → already complete, mark and skip
- **In progress but currentMinutes >= required for any drop** → that drop done, claim it, keep farming the others

Earlier drafts of this spec called for `scrubStaleCompleted` to un-mark
campaigns based on dashboard's `claimed: false`. That was abandoned —
dashboard lies, scrub fights the poll, infinite loop. Replaced by the
`InInventory` check below.

**`autoClaimAndMarkCompleted` rule** (replaces both old logic + the dropped scrub):

```go
for _, c := range campaigns {
    // ... eligibility checks ...
    // Auto-claim ready drops (existing logic).
    // Then:
    if !c.InInventory {
        // Not in dropCampaignsInProgress → user has fully claimed it
        // (or never had progress). Either way: skip from now on.
        f.cfg.MarkCampaignCompleted(c.ID)
        f.cfg.Save()
    }
}
```

**`pollDropProgressOnce` rule** (poll handler): when poll says
`current >= required`, do NOT mark the campaign completed. Instead trigger
an out-of-cycle `processDrops` so the inventory pull runs and the
`InInventory` check evaluates correctly. Multi-drop campaigns where one
drop is done but others remain stay un-completed.

(Original "scrubStaleCompleted before selector" content kept below for
historical reference; do not implement it):

```go
// (HISTORICAL — replaced by InInventory check above)
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
```

Needs new `Config.UnmarkCampaignCompleted(id)` helper.

### 6.5 Stall-detection baseline must come from inventory cycles only

`applyStallDetection` compares the new cycle's drop progress against
`lastPickProgress` to decide if the picked channel is stalled. Therefore
`lastPickProgress` MUST be the snapshot from the previous inventory cycle —
not the latest live value from WebSocket / poll.

`applyDropProgressUpdate` (called from PubSub drop-progress events AND from
`pollDropProgressOnce`) updates the in-memory `activeDrops` for UI freshness.
It MUST NOT mutate `lastPickProgress`. Only `snapshotPickProgress` (called at
the end of each `processDrops`) writes to `lastPickProgress`.

If we let live updates rewrite the baseline, a healthy channel that earns
5 min between cycles will appear stalled at the next 15-min check (because
inventory might still report the stale value, or the same value).

### 6.6 Multi-drop progress matching uses (CampaignID, DropID)

`applyDropProgressUpdate` receives both `CampaignID` and `DropID` in the
`DropProgressData` payload. To avoid showing one drop's progress against
another drop's required-minutes (e.g. Marble Day's DROP 1 = 30 min vs
DROP 6 = 720 min), match the in-memory `activeDrops` entry by **both**
fields. CampaignID-only match is a v1.8.0 implementation bug.

When the matched ActiveDrop's required differs from the payload's required,
trust the payload (Twitch's session reports the currently-earning drop,
which may have advanced past the one we cached at last inventory cycle).

### 7. Polling cadence

`dropCheckLoop` ticker reduced from 5 min → 15 min. WebSocket carries the
real-time load; the polling cycle exists only as a safety net for missed
events / WebSocket disconnects. The startup polling still runs at 30s for
fast initial pick.

## Architecture

### Files touched

| File | Action | Notes |
|---|---|---|
| `internal/twitch/pubsub.go` | Modify | Add topic patterns for `user-drop-events` and `broadcast-settings-update`; route messages to new event types |
| `internal/twitch/types.go` | Modify | Add new event payloads (e.g. `EventDropProgress`, `EventDropClaim`, `EventBroadcastSettings`) |
| `internal/farmer/farmer.go` | Modify | New event handlers for the 3 new event types; `applySelectorPick` now also manages per-channel topic subscriptions; `dropCheckLoop` ticker → 15min |
| `internal/farmer/drops.go` | Modify | New `scrubStaleCompleted` helper (called at start of `processDrops`); `applySelectorPick` extended to call `f.subscribeChannelTopics` / `f.unsubscribeChannelTopics`; new method `f.applyDropProgressUpdate(dropID, minutes)` for WebSocket-driven local update |
| `internal/farmer/dropselector.go` | Modify | `sortPool` uses `wanted_games` rank as primary key; falls back to remaining_time when list is empty |
| `internal/farmer/dropselector_test.go` | Modify | New tests for `wanted_games` sort + empty fallback |
| `internal/config/config.go` | Modify | New `GamesToWatch` field + 5 helpers; new `UnmarkCampaignCompleted` helper |
| `internal/config/config_test.go` | Modify | Tests for the new helpers |
| `internal/ui/app.go` | Modify | New `inputGameList` modal state; `g` keybind handler; modal renderer |
| `internal/ui/components.go` | Modify | New `renderGameListEditor` function; help bar gains `g games` and loses `i pin` |
| `internal/web/server.go` | Modify | New `GET/PUT /api/wanted_games` endpoints; pin endpoint stays but is no-op-marked-deprecated |
| `internal/web/static/index.html` | Modify | Drops table loses pin button; new "Wanted Games" collapsible section with reorder UI |
| `cmd/twitchpoint/main.go` | Modify | Version bump 1.7.0 → 1.8.0 |
| `changelog.txt` | Modify | New entry |

### Data flow

```
                ┌──────────────────────────────┐
                │ Twitch PubSub WebSocket       │
                │ wss://pubsub-edge.twitch.tv/v1│
                └──────────┬────────────────────┘
                           │
        ┌──────────────────┼──────────────────────────┐
        │                  │                          │
   user-drop-events   broadcast-settings        video-playback
        │                  │                          │
        ▼                  ▼                          ▼
  ┌──────────┐     ┌────────────────┐         ┌────────────────┐
  │ progress │     │ game changed?  │         │ stream up/down │
  │ → local  │     │ → mark stall + │         │ → re-run       │
  │   update │     │   re-run       │         │   selector     │
  │ claim    │     │   selector     │         │   immediately  │
  │ → invent │     └────────────────┘         └────────────────┘
  │   pull + │
  │   re-run │
  └─────┬────┘
        │
        ▼
   [Selector picks new channel via wanted_games sort]
        │
        ▼
   [applySelectorPick subscribes to new channel's topics,
    unsubscribes from previous channel's topics]
```

### WebSocket subscription lifecycle

- **At Farmer start:** subscribe `user-drop-events.<userID>` (1× user-level topic).
- **When selector picks channel X:**
  - If X is a new temp channel: `addTemporaryChannel` already subscribes to `video-playback-by-id.X` and `raid.X`. Add `broadcast-settings-update.X` here.
  - If X was already tracked: subscribe to `broadcast-settings-update.X` if not already.
- **When previous pick Y differs from new pick X:** unsubscribe `broadcast-settings-update.Y` (Y still kept for `video-playback-by-id` because it's a configured/temp channel).
- **When Y is removed via `removeTemporaryChannel`:** all topics unsubscribed (existing behavior).

PubSub topic add/remove is rate-limited Twitch-side; current `pubsub.go` already batches LISTEN frames. Reuse same.

### Sort-key implementation

```go
type sortKey struct {
    gameRank    int       // index in wanted_games, len(wanted_games) if absent
    earliestEnd time.Time // soonest endAt across this channel's campaigns
    viewerCount int       // for desc tie-break
}

// In sortPool:
wanted := s.cfg.GetGamesToWatch()
gameRanks := make(map[string]int, len(wanted))
for i, g := range wanted {
    gameRanks[strings.ToLower(g)] = i
}
fallbackToTime := len(wanted) == 0

for _, e := range pool {
    // gameRank = best (lowest index) across ALL campaigns this channel serves.
    // A channel that serves both Marvel Rivals (rank 1) and Honkai (rank 3)
    // gets gameRank=1 — we want to farm it because it covers our top game.
    bestRank := len(wanted) // default: no wanted-game match
    var earliestEnd time.Time
    first := true
    for _, ref := range e.Campaigns {
        if !fallbackToTime {
            if r, ok := gameRanks[strings.ToLower(ref.GameName)]; ok {
                if r < bestRank {
                    bestRank = r
                }
            }
        }
        if first || ref.EndAt.Before(earliestEnd) {
            earliestEnd = ref.EndAt
            first = false
        }
    }
    keys[e] = sortKey{gameRank: bestRank, earliestEnd: earliestEnd, viewerCount: e.ViewerCount}
}
sort.SliceStable(pool, func(i, j int) bool {
    if !fallbackToTime && keys[pool[i]].gameRank != keys[pool[j]].gameRank {
        return keys[pool[i]].gameRank < keys[pool[j]].gameRank
    }
    if !keys[pool[i]].earliestEnd.Equal(keys[pool[j]].earliestEnd) {
        return keys[pool[i]].earliestEnd.Before(keys[pool[j]].earliestEnd)
    }
    return keys[pool[i]].viewerCount > keys[pool[j]].viewerCount
})
```

## Migration & cutover

**Config migration:** zero. Existing v1.7.0 configs load with empty `GamesToWatch` → bot behaves as v1.7.0 (remaining_time sort). User can opt into the new behavior by adding games via TUI/web.

**Pin field:** stays as `PinnedCampaignID` in JSON for backward compat; selector ignores it. UI no longer offers pin action. Documented as deprecated in changelog.

**Cutover steps:**
1. Config helpers + tests
2. PubSub topic patterns + event-type additions
3. WebSocket message handlers (drop-progress, drop-claim, broadcast-settings-update)
4. Selector sort-key change with empty-fallback + tests
5. `applySelectorPick` topic-subscription delta
6. Daily-rolling completed-scrub helper
7. Reduce poll cadence
8. TUI modal editor
9. Web UI changes
10. Version bump + changelog
11. Manual smoke test
12. Tag + GH release with binaries

**Rollback:** previous v1.7.0 binaries in GH release page; config is forward-compatible (new fields ignored by older versions).

## Test plan

| Scenario | Expected |
|---|---|
| Bot start with empty `wanted_games` | v1.7.0 behavior — sort by remaining_time |
| User adds "Arena Breakout: Infinite" via TUI `g` → `+` | Persisted to config, next selector cycle prioritizes ABI channels |
| User reorders 3 games via `g` → `↑/↓` | Selector follows new order on next pick |
| Streamer mid-stream switches ABI → Just Chatting | Within seconds: bot detects via `broadcast-settings-update`, marks 15 min cooldown for that channel, picks new |
| Twitch credits drop minute via PubSub | UI progress jumps within 1s, no GQL call made |
| Twitch claims a drop via PubSub | Bot calls `ClaimDrop` async, refreshes inventory, picks next campaign if all done |
| All drops claimed for active campaign | `drop-claim` triggers inventory pull → campaign auto-marked completed → selector picks next |
| Marble Day daily-rolling reset | At next inventory pull, `scrubStaleCompleted` removes Marble Day from `CompletedCampaigns`, it becomes eligible again |
| WebSocket disconnects | Polling fallback (15 min) keeps bot functional; reconnect attempts ongoing |
| Backward-compat: load v1.7.0 config with `pinned_campaign_id` set | Loads cleanly, pin is silently ignored |

## Acceptance criteria

1. `go build ./...` and `go vet ./...` exit 0
2. New unit tests for selector sort-key (wanted_games) and config helpers PASS
3. Live verification:
   - WebSocket connects and `[PubSub] subscribed to user-drop-events.<id>` log appears
   - When Twitch credits a drop minute, `[Drops/WS] progress: <drop> +<n> minutes` log within 2 sec
   - When watched streamer changes game, `[Drops/WS] %s switched game (%s → %s)` log within 5 sec, followed by selector re-run
   - TUI `g` opens game list editor, navigation/add/remove/reorder all work, config persists
4. Existing user-facing logs preserved: `[Drops/Pool] picked X`, `[Drops/Pool] empty pool`, `[Drops/Pool] no credit on X — Nm cooldown`, channel-points `+10 points on X (WATCH)`

## Out of scope for v1.8.0

- Per-game settings (skip-after-N-seconds, exclusion list) — keep TDM-simple
- WebSocket-only mode (kill the polling fallback) — too risky
- Game name autocomplete from Twitch's full game directory — autocomplete only against current eligible campaigns (good enough)
- Removing the `PinnedCampaignID` config field — defer to v2.0.0 if ever
- Drop notifications (Discord/desktop) — separate feature

## Files touched (estimate)

| File | Lines today | Lines after | Delta |
|---|---|---|---|
| internal/twitch/pubsub.go | ~250 | ~310 | +60 |
| internal/twitch/types.go | ~150 | ~180 | +30 |
| internal/farmer/farmer.go | ~1080 | ~1170 | +90 |
| internal/farmer/drops.go | ~620 | ~680 | +60 |
| internal/farmer/dropselector.go | ~277 | ~310 | +33 |
| internal/farmer/dropselector_test.go | ~480 | ~570 | +90 |
| internal/config/config.go | ~322 | ~360 | +38 |
| internal/config/config_test.go | ~88 | ~150 | +62 |
| internal/ui/app.go | ~485 | ~590 | +105 |
| internal/ui/components.go | ~340 | ~390 | +50 |
| internal/web/server.go | ~395 | ~440 | +45 |
| internal/web/static/index.html | ~1050 | ~1130 | +80 |
| cmd/twitchpoint/main.go | 1 line const | 1 line const | 0 (version) |
| changelog.txt | append | append | +1 entry |

Net new code: ~750 lines added (mostly TUI editor + WebSocket handlers + tests).
