# TwitchPoint Farmer

Automated Twitch channel points farmer. Watches multiple channels, auto-claims bonuses, joins raids, earns watch-time points, and mines Twitch Drops — all from a single binary.

## Features

- **Auto-Claim Bonuses** — Claims channel point bonuses the moment they appear (3-retry async via PubSub)
- **Auto-Join Raids** — Joins raids for bonus points (dedup'd against PubSub spam)
- **Watch-Time Points** — Legacy `spade.twitch.tv/track` POST heartbeats for the 2 rotation slots
- **Twitch Drops** — GraphQL `sendSpadeEvents` heartbeats for the picked drop channel; auto-selects from game directory or campaign allow-list; auto-claims completed drops
- **Wanted Games Priority** — Ordered list of games to prefer; account-linked campaigns NOT in the list are still farmed and shown with an `[Auto]` marker
- **Tabbed TUI** — Channels / Drops / Help tabs with keyboard navigation
- **Tabbed Web Dashboard** — Same three tabs, with Twitch-catalog autocomplete + drag-reorder
- **Windows System Tray** — Tray icon with live stats, hide/show console, auto-start
- **Update Notifications** — Get notified when a new version is available
- **Zero Dependencies** — Single binary, no external services

## Quick Start

Download the latest release for your platform from [Releases](https://github.com/miwidot/twitchpoint/releases).

| File | Platform |
|------|----------|
| `twitchpoint-macos` | macOS Apple Silicon (M1/M2/M3/M4) |
| `twitchpoint-macos-intel` | macOS Intel (x86_64) |
| `twitchpoint-linux` | Linux x86_64 |
| `twitchpoint-linux-arm64` | Linux ARM64 (Raspberry Pi 4/5, ARM servers) |
| `twitchpoint-windows.exe` | Windows x86_64 |

```bash
# macOS Apple Silicon
chmod +x twitchpoint-macos && ./twitchpoint-macos

# macOS Intel
chmod +x twitchpoint-macos-intel && ./twitchpoint-macos-intel

# Linux
chmod +x twitchpoint-linux && ./twitchpoint-linux

# Windows
twitchpoint-windows.exe
```

On first run you'll log in via Twitch Device Code OAuth — open the link, enter the code, done.
Channels can be added from the TUI (`a` key) or the Web UI.

### Build from Source

```bash
git clone https://github.com/miwidot/twitchpoint.git
cd twitchpoint
make build        # current platform
make build-all    # macOS, Linux, Windows
./bin/twitchpoint
```

## Configuration

Config file `config.json` is created automatically on first run.

```json
{
  "auth_token": "auto-obtained-via-oauth",
  "channel_configs": [
    { "login": "channelname", "priority": 1 },
    { "login": "otherchannel", "priority": 2 }
  ],
  "web_enabled": true,
  "web_port": 8080,
  "irc_enabled": true,
  "drops_enabled": true
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `auth_token` | — | Twitch OAuth token (auto-obtained on first run) |
| `channel_configs` | `[]` | Channels to watch with priority (1 or 2) |
| `web_enabled` | `true` | Enable web dashboard |
| `web_port` | `8080` | Web server port |
| `irc_enabled` | `true` | IRC presence for active viewer status |
| `drops_enabled` | `true` | Automatic drop campaign mining |
| `disabled_campaigns` | `[]` | Campaign IDs to skip (managed via TUI Drops tab `Space` or Web UI toggle) |
| `completed_campaigns` | `[]` | Campaign IDs auto-marked completed (managed automatically) |
| `games_to_watch` | `[]` | Ordered priority list of game names. Empty = no preference (v1.7.0 behavior); non-empty = wanted games sort first, others tagged `[Auto]` |

### Priority System

Twitch only credits watch-time points for **2 channels simultaneously** through the legacy POST endpoint (the picked drop channel runs separately on the GraphQL pipeline and doesn't count against this limit).

- **P0 (Drop Active)** — Auto-promoted when a drop campaign is being farmed. Highest priority.
- **P1 (Always Watch)** — Holds a Spade slot permanently. Use for your most important channels.
- **P2 (Rotate)** — Cycles every 5 minutes. All other channels share the remaining slots.

The drops Watcher's currently-picked channel is **explicitly skipped** by the points rotation to avoid double-tracking on both pipelines.

## Terminal UI

Three tabs: **Channels** / **Drops** / **Help**.

### Tab Navigation (works in every tab)

| Key | Action |
|-----|--------|
| `1` / `2` / `3` | Switch directly to Channels / Drops / Help |
| `Tab` / `Shift+Tab` | Cycle tabs forward / backward |
| `q` / `Ctrl+C` | Quit |

### Channels Tab

| Key | Action |
|-----|--------|
| `a` | Add channel (text-input modal) |
| `d` | Remove channel (text-input modal) |
| `p` | Set priority (`channelname 1` or `channelname 2`) |
| `↑` / `k` | Scroll channel table up |
| `↓` / `j` | Scroll channel table down |
| `Home` | Jump to top of channel table |
| `End` | Jump to bottom of channel table |

### Drops Tab

A unified `j`/`k` cursor moves through three stacked panels (Drop Campaigns → Wanted Games → Settings). The cursor overflows panel boundaries — pressing `j` past the last campaign jumps to the first wanted game, and so on.

| Key | Action |
|-----|--------|
| `↑` / `k` | Move cursor up (overflows to previous panel) |
| `↓` / `j` | Move cursor down (overflows to next panel) |
| `Space` | Toggle (Drop Campaign enable/disable, or Setting on/off) |
| `+` | Add wanted-game (opens text prompt with **live Twitch catalog autocomplete**) |
| `-` | Remove the wanted-game under the cursor |
| `u` | Reorder wanted-game up |
| `d` | Reorder wanted-game down |

`+` / `-` / `u` / `d` auto-focus the Wanted Games panel — no need to navigate there first.

#### Wanted-Game Add Prompt

| Key | Action |
|-----|--------|
| Type 2+ chars | 250ms-debounced search against Twitch's full game catalog |
| `↑` / `↓` | Navigate suggestion list |
| `Enter` | Save selected suggestion (cursor ≥0) or typed text verbatim (cursor =-1) |
| `Esc` | Cancel without saving |

Game names are case-preserved (Twitch's selector is case-sensitive — `Escape from Tarkov` ≠ `escape from tarkov`).

### Help Tab

Read-only static reference for all of the above plus a pipelines-explainer.

## Web Dashboard

Available at `http://localhost:8080` by default. Same three tabs as the TUI, with mouse interactions where applicable (drag-reorder for wanted games, click toggles for campaigns) and **keyboard shortcuts** for fast power-user navigation.

### Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `1` / `2` / `3` | Switch directly to Channels / Drops / Help |
| `Tab` / `Shift+Tab` | Cycle tabs forward / backward |
| `/` | Jump to Drops tab and focus the games-search input |
| `n` | Open the Add Channel modal |
| `Esc` | Close any open modal / cancel input |
| `↑` / `↓` | Navigate Twitch-catalog suggestions in the games search |
| `Enter` | Save selected suggestion or typed text in the games search |

### Tab Contents

- **01 Channels** — Active drop strip (campaign + game + channel + chartreuse progress bar) → Streams table (priority, status, game with drop %, balance, earned, claims; hover row reveals action buttons for priority toggle + remove) → Event log (color-coded by event type) → Stats footer with count-up animations
- **02 Drops** — Drop Campaigns table (inline enable/disable toggle, `[Auto]` tag for non-wanted_games campaigns, status pills) → Wanted Games (drag-reorder, Twitch-catalog autocomplete) → Settings (placeholder; runtime toggles coming)
- **03 Help** — Keyboard reference, status glyph legend, drops-vs-channel-points pipeline explainer

Auto-refreshes every 5 seconds (parallel fetch of all `/api/*` endpoints).

## Twitch Drops

When `drops_enabled` is `true`, TwitchPoint automatically:

1. **Polls inventory every 15 minutes** (PubSub `user-drop-events` carries the real-time progress; the inventory poll is a safety net)
2. **Polls `DropCurrentSession` every 60 seconds** for the picked drop channel
3. **Matches eligible campaigns** to channels — for ACL/Partner-Only campaigns it queries the `allowed_channels` list directly, for open campaigns it pulls the top 100 drops-enabled streams of the game directory
4. **Auto-selects a live channel** even if it's not in your config (it's added as a temp channel for the duration of the pick)
5. **Skips campaigns** where your account is not linked to the game, where the campaign is disabled by you, completed, or has no earnable drops in the current time window
6. **Auto-claims** completed drops synchronously (the local `IsClaimed` flag is mutated in-place to prevent re-pick loops on multi-drop campaigns)
7. **Fails over** to another channel if the current pick goes offline, changes game (with a 30s debounce so flapping streamers don't cause unnecessary churn), or stops crediting minutes (silent-pick threshold = 3 minutes)

### Wanted Games (priority)

Set an ordered list of games you want farmed first. The selector still considers ALL account-linked campaigns (those NOT in the wanted list still run as fallback) — the priority just decides which goes first when multiple are eligible.

- **Empty wanted list** → all eligible campaigns run, ordered by `endAt` + viewer count
- **With entries** → wanted games sort first; non-wanted are tagged `[Auto]` in the UI

### Status Indicators

| Status | Meaning |
|--------|---------|
| `ACTIVE` | Currently being farmed (the picked channel) |
| `QUEUED` | In the selector pool, ranked behind ACTIVE |
| `IDLE` | Eligible but no live channels right now |
| `DISABLED` | User-disabled via TUI Space-toggle or Web UI toggle |
| `COMPLETED` | All watchable drops claimed |
| `[Auto]` tag | Account-linked but NOT in your wanted_games list |

## Windows System Tray

On Windows, a system tray icon runs alongside the TUI:

- **Left-click** the tray icon to open the menu
- **Live stats** — Points, claims, channels, and drops update every 5s
- **Open Web UI** — Opens the dashboard in your browser
- **Hide/Show Console** — Toggle the TUI window. Hidden = runs silently in the tray
- **Start with Windows** — Toggle auto-start on login (registry-based)
- **Quit** — Clean shutdown

## Docker

Runs in **headless mode** — no TUI, only the farmer + Web UI. First-run login works via `docker logs`.

```yaml
# docker-compose.yml
services:
  twitchpoint:
    build: .
    container_name: twitchpoint
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./config:/app/config
    environment:
      - TZ=Europe/Berlin
```

```bash
docker-compose up -d

# First run: check logs for the login code
docker-compose logs -f
# Open the URL shown, enter the code, authorize — token is saved to config volume

# After login, manage everything via Web UI at http://localhost:8080
```

You can also set a token manually before starting:

```bash
cp config/config.example.json config/config.json
# Edit config/config.json with your auth token and channels
docker-compose up -d
```

## CLI Flags

```
./twitchpoint [flags]

  --config string      Path to config file (default: config.json)
  --add-channel string Add a channel and exit
  --token string       Set auth token manually and exit
  --login              Force re-login via Device Code OAuth
  --headless           Run without TUI (for Docker/servers)
```

## How It Works

Two **independent** credit pipelines run side by side. Routing the wrong heartbeat to the wrong endpoint silently fails the credit (verified the hard way more than once).

1. **OAuth** — Twitch Android Client-ID with Device Code flow (no browser automation, no CAPTCHA)
2. **PubSub** — WebSocket for real-time events: bonus claims (`community-points-user-v1`), drop progress (`user-drop-events`), stream up/down (`video-playback-by-id`), raids (`raid`), broadcast settings updates
3. **Channel-Points pipeline** — Legacy `POST spade.twitch.tv/track` with form-encoded base64-JSON payload. Used by the 2 rotation slots.
4. **Drops pipeline** — GraphQL `sendSpadeEvents` mutation with gzip+base64 payload. INT `user_id`, non-empty `game_id`, exact game name required (Twitch silently drops credit on type/value mismatch). Used exclusively by the picked drop channel.
5. **IRC** — Chat-only TLS connection for active viewer presence (no commands sent)
6. **GQL** — Inventory polls, channel info, claim mutations, raid joins, game-directory queries

### Internal Architecture (v2.0)

- `internal/twitch/` — GQL client, PubSub, Spade tracker, StreamProber, IRCClient, raw types
- `internal/channels/` — Channel registry + state + immutable snapshots
- `internal/drops/` — Drops Service (Selector + StallTracker + Watcher + auto-claim)
- `internal/points/` — Channel-points Service (rotation, balance refresh, event handlers, dedup, IRC lifecycle)
- `internal/farmer/` — Thin orchestrator (829 lines) — bring-up, channel lifecycle, event dispatch
- `internal/ui/` — Bubbletea TUI (Channels / Drops / Help tabs)
- `internal/web/` — HTTP server + embedded single-page dashboard

## License

MIT — see [LICENSE](LICENSE)

## Disclaimer

This tool is for educational purposes. Use at your own risk. Not affiliated with Twitch.
