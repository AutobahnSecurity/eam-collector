# eam-collector

Lightweight client-side agent that tracks AI tool usage on developer machines and sends session data to an EAM server for governance, DLP, and compliance analysis.

Runs as a background daemon (launchd on macOS, systemd on Linux). Single binary, no dependencies.

## Supported Tools

| Tool | Parser | Local Data |
|------|--------|-----------|
| Claude Code | `claude` | JSONL session files (`~/.claude/projects/`) |
| Claude Desktop (code/cowork) | `claude` | Desktop session metadata → JSONL |
| Claude Desktop (chat) | `claude` | tipTap editor snapshots in IndexedDB LevelDB |

The unified `claude` parser handles all Claude surfaces. Legacy config names `claude_code` and `claude_desktop` are accepted as aliases.

## Quick Start

### 1. Install via Homebrew

```bash
brew tap AutobahnSecurity/tap
brew install eam-collector
```

This installs the binary, creates a config template at `~/.eam-collector/config.yaml`, and registers a background service.

### 2. Configure

```bash
nano ~/.eam-collector/config.yaml
```

Set your EAM server URL and API key:

```yaml
server:
  url: https://eam.your-company.com
  api_key: ek_your-ingest-key

interval: 60
lookback: 24

parsers:
  claude:
    enabled: true
```

### 3. Start

```bash
brew services start eam-collector
```

### 4. Verify

```bash
# Check logs
tail -f /opt/homebrew/var/log/eam-collector.log

# Check if running
pgrep -f eam-collector
```

## Manual Install (without Homebrew)

For Linux or environments without Homebrew, download the binary from [Releases](https://github.com/AutobahnSecurity/eam-collector/releases) and use the install script:

```bash
tar xzf eam-collector-*-linux-amd64.tar.gz
sudo ./install/install.sh
```

This installs the binary to `/usr/local/bin/eam-collector` and sets up:
- **macOS**: LaunchAgent (`~/Library/LaunchAgents/com.eam.collector.plist`)
- **Linux**: systemd service (`/etc/systemd/system/eam-collector.service`)

## Configuration

| Key | Default | Description |
|-----|---------|------------|
| `server.url` | (required) | EAM server URL |
| `server.api_key` | (required) | Ingest API key (`ek_*` format) |
| `interval` | `60` | Polling interval in seconds (minimum 10) |
| `lookback` | `24` | Only read sessions modified within this many hours |
| `parsers.<name>.enabled` | `false` | Enable/disable individual tool parsers |

Environment variables are expanded in config values (e.g., `api_key: ${EAM_API_KEY}`).

## How It Works

1. **Poll** enabled parsers every `interval` seconds
2. **Read** new/changed session data from local tool files
3. **Detect** Claude account identity (org UUID from statsig cache)
4. **Send** records to EAM server `POST /api/ingest` with device ID + identities
5. **Save** state (file offsets, timestamps) after each cycle to prevent duplicate sends

### Lookback Filter

The `lookback` setting limits which files are processed. Only sessions modified within the lookback window are read. Set to `2` for "active sessions only" mode -- no historical backlog on first run.

### Identity Detection

The collector extracts Claude account/organization UUIDs from two sources: the statsig cache (`~/.claude/statsig/`) for standalone CLI sessions, and Desktop session directory paths (`~/Library/Application Support/Claude/*/`) for Desktop sessions. Per-session identity is snapshotted when a session is first seen.

These are sent as `identities[]` in the ingest payload. The EAM server checks if any org UUID matches `GOVERNED_ORG_IDS` to determine governance.

### State Persistence

State is saved to `~/.eam-collector/state.json` (mode 600) after each collection cycle. Parser offsets are always persisted to prevent duplicate record collection, even if some sends fail.

Per-parser state:
- **Claude (code/cowork)**: byte offsets per JSONL file, known file set
- **Claude (chat)**: last processed tipTap timestamp

## Uninstall

### Homebrew

```bash
brew services stop eam-collector
brew uninstall eam-collector
rm -rf ~/.eam-collector
```

### Manual

```bash
# macOS
launchctl unload ~/Library/LaunchAgents/com.eam.collector.plist
rm ~/Library/LaunchAgents/com.eam.collector.plist
sudo rm /usr/local/bin/eam-collector
rm -rf ~/.eam-collector

# Linux
sudo systemctl stop eam-collector
sudo systemctl disable eam-collector
sudo rm /etc/systemd/system/eam-collector.service
sudo rm /usr/local/bin/eam-collector
rm -rf ~/.eam-collector
```

## Development

```bash
make build                                              # build for current platform
make build-all                                          # all platforms
make test                                               # run tests
./dist/eam-collector -config config.example.yaml        # run locally
```

### Releasing

Tag a version to trigger a release via GoReleaser:

```bash
git tag v0.1.0
git push origin v0.1.0
```

This builds binaries, creates a GitHub Release, and pushes the Homebrew formula to [AutobahnSecurity/homebrew-tap](https://github.com/AutobahnSecurity/homebrew-tap).

## License

Proprietary - Autobahn Security
