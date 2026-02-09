# TwitchPoint Farmer

Automated Twitch channel points farmer with web interface. Watches multiple channels, auto-claims bonuses, joins raids, and earns watch-time points.

## Features

- **Auto-Claim Bonuses** - Automatically claims channel point bonuses when available
- **Auto-Join Raids** - Joins raids to earn bonus points
- **Watch-Time Points** - Earns points through Spade heartbeat tracking
- **IRC Presence** - Connects to Twitch IRC for active viewer status
- **Priority System** - P1 channels always watched, P2 channels rotate (Twitch limits to 2 concurrent)
- **Web Interface** - Remote monitoring and control via browser
- **Terminal UI** - Local TUI with real-time stats and logs
- **Zero Dependencies** - Single binary, no external services required

## Quick Start

### Option 1: Download Binary

Download the latest release for your platform from [Releases](https://github.com/miwidot/twitchpoint/releases).

```bash
# Make executable (macOS/Linux)
chmod +x twitchpoint-*

# Run
./twitchpoint-macos    # macOS
./twitchpoint-linux    # Linux
./twitchpoint-windows.exe  # Windows
```

On first run, you'll be prompted to login via Twitch Device Code OAuth.

### Option 2: Docker

```bash
# Clone repo
git clone https://github.com/miwidot/twitchpoint.git
cd twitchpoint

# Create config
cp config/config.example.json config/config.json
# Edit config/config.json with your auth token and channels

# Start with docker-compose
docker-compose up -d

# View logs
docker-compose logs -f

# Open web UI
open http://localhost:8080
```

### Option 3: Build from Source

```bash
# Clone
git clone https://github.com/miwidot/twitchpoint.git
cd twitchpoint

# Build
make build

# Run
./bin/twitchpoint
```

## Configuration

Config file: `config.json` (created on first run)

```json
{
  "auth_token": "your_oauth_token",
  "channel_configs": [
    {"login": "channelname", "priority": 1},
    {"login": "otherchannel", "priority": 2}
  ],
  "web_enabled": true,
  "web_port": 8080,
  "irc_enabled": true
}
```

### Priority System

- **P1 (Priority 1)**: Always watched - holds Spade slot permanently
- **P2 (Priority 2)**: Rotation - cycles every 5 minutes

Twitch only credits watch points for 2 channels simultaneously, so P1 channels get preference.

### Config Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth_token` | string | - | Twitch OAuth token (auto-obtained via Device Code) |
| `channel_configs` | array | [] | Channels to watch with priority |
| `web_enabled` | bool | false | Enable web interface |
| `web_port` | int | 8080 | Web server port |
| `irc_enabled` | bool | true | Enable IRC for viewer presence |

## Web Interface

Enable with `web_enabled: true` in config.

Features:
- Dashboard with stats (uptime, points earned, claims)
- Channel table with status, balance, earned points
- Add/remove channels
- Toggle priority (click P1/P2 button)
- Auto-refresh every 5 seconds

![Web UI](docs/webui.png)

## Terminal UI

Keyboard shortcuts:

| Key | Action |
|-----|--------|
| `q` | Quit |
| `a` | Add channel |
| `d` | Delete channel |
| `p` | Set priority |
| `↑/↓` | Scroll logs |

## Docker

### docker-compose (recommended)

```yaml
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
```

### Manual Docker

```bash
# Build
docker build -t twitchpoint .

# Run
docker run -d \
  --name twitchpoint \
  -p 8080:8080 \
  -v $(pwd)/config:/app/config \
  --restart unless-stopped \
  twitchpoint
```

## CLI Flags

```bash
./twitchpoint [flags]

Flags:
  --config string      Path to config file (default: config.json)
  --add-channel string Add a channel and exit
  --token string       Set auth token manually and exit
  --login              Force re-login via Device Code OAuth
```

## How It Works

1. **Authentication**: Uses Twitch TV Client-ID with Device Code OAuth flow
2. **PubSub**: WebSocket connection for real-time events (stream up/down, points, raids)
3. **Spade**: Minute-watched heartbeats to earn watch-time points
4. **IRC**: Chat presence for active viewer status
5. **GQL**: GraphQL API for claims, raids, and channel info

## Building

```bash
# Build for current platform
make build

# Build for all platforms
make build-all

# Clean
make clean
```

## License

MIT License - see [LICENSE](LICENSE)

## Disclaimer

This tool is for educational purposes. Use at your own risk. Not affiliated with Twitch.
