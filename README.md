# TwitchPoint Farmer

Automated Twitch channel points farmer. Watches multiple channels, auto-claims bonuses, joins raids, earns watch-time points, and mines Twitch Drops — all from a single binary.

## Features

- **Auto-Claim Bonuses** — Claims channel point bonuses the moment they appear
- **Auto-Join Raids** — Joins raids for bonus points
- **Watch-Time Points** — Spade heartbeat tracking earns watch-time points
- **Twitch Drops** — Tracks drop campaigns, auto-selects channels, claims rewards
- **Priority System** — P1 channels always watched, P2 channels rotate every 5 min
- **Web Dashboard** — Monitor and control everything from a browser
- **Terminal UI** — Real-time TUI with stats, channels, event log
- **Windows System Tray** — Tray icon with live stats, hide/show console, auto-start
- **Update Notifications** — Get notified when a new version is available
- **Zero Dependencies** — Single binary, no external services

## Quick Start

Download the latest release for your platform from [Releases](https://github.com/miwidot/twitchpoint/releases).

```bash
# macOS / Linux
chmod +x twitchpoint-*
./twitchpoint-macos     # or ./twitchpoint-linux

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
| `disabled_campaigns` | `[]` | Campaign IDs to skip (managed via Web UI) |

### Priority System

Twitch only credits watch-time points for **2 channels simultaneously**.

- **P1 (Always Watch)** — Holds a Spade slot permanently. Use for your most important channels.
- **P2 (Rotate)** — Cycles every 5 minutes. All other channels share the remaining slots.
- **P0 (Drop Active)** — Auto-promoted when a drop campaign requires watching. Temporary, takes highest priority.

## Terminal UI

The TUI shows real-time channel status, points, event log, and drop progress.

| Key | Action |
|-----|--------|
| `a` | Add channel |
| `d` | Delete channel |
| `p` | Set priority (`channelname 1` or `channelname 2`) |
| `q` | Quit |

## Web Dashboard

Available at `http://localhost:8080` by default. Features:

- Live stats: uptime, total points, claims, active drops
- Channel table with online status, balance, earned points, drop progress
- Add/remove channels, toggle priority
- Drop Campaigns table with enable/disable toggle
- Auto-refreshes every 5 seconds

## Twitch Drops

When `drops_enabled` is `true`, TwitchPoint automatically:

1. Checks your drop inventory every 5 minutes
2. Matches campaigns to your channels (by allowed list or game category)
3. Auto-selects a live channel if none of your channels qualify
4. Claims completed drops
5. Fails over to another channel if the current one goes offline or raids

Disable individual campaigns from the Web UI drop table.

## Windows System Tray

On Windows, a system tray icon runs alongside the TUI:

- **Left-click** the tray icon to open the menu
- **Live stats** — Points, claims, channels, and drops update every 5s
- **Open Web UI** — Opens the dashboard in your browser
- **Hide/Show Console** — Toggle the TUI window. Hidden = runs silently in the tray
- **Start with Windows** — Toggle auto-start on login (registry-based)
- **Quit** — Clean shutdown

## Docker

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
# Create config first
cp config/config.example.json config/config.json
# Edit with your auth token and channels

docker-compose up -d
docker-compose logs -f
```

## CLI Flags

```
./twitchpoint [flags]

  --config string      Path to config file (default: config.json)
  --add-channel string Add a channel and exit
  --token string       Set auth token manually and exit
  --login              Force re-login via Device Code OAuth
```

## How It Works

1. **OAuth** — Twitch TV Client-ID with Device Code flow (no browser automation)
2. **PubSub** — WebSocket for real-time events (stream up/down, bonus points, raids)
3. **Spade** — Minute-watched heartbeats for watch-time point earnings
4. **IRC** — Chat connection for active viewer presence
5. **GQL** — GraphQL API for claims, raids, drops inventory, and channel info

## License

MIT — see [LICENSE](LICENSE)

## Disclaimer

This tool is for educational purposes. Use at your own risk. Not affiliated with Twitch.
