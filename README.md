# eam-collector

Lightweight client-side agent that tracks AI tool usage on developer machines and sends session data to an EAM server for governance, DLP, and compliance analysis.

Runs as a background daemon (launchd on macOS, systemd on Linux). Single binary, no dependencies.

## Supported Tools

| Tool | Parser | Local Data |
|------|--------|-----------|
| Claude Code | `claude_code` | JSONL session files (`~/.claude/projects/`) |
| Cursor | `cursor` | SQLite database (`state.vscdb`) |
| GitHub Copilot | `copilot` | JSON chat sessions |
| Continue.dev | `continuedev` | JSON session files |

## Quick Start

### 1. Build

```bash
make build          # current platform
make build-all      # all platforms (macOS arm64/amd64, Linux amd64, Windows)
```

### 2. Configure

```bash
mkdir -p ~/.eam-collector
cat > ~/.eam-collector/config.yaml <<EOF
server:
  url: https://eam.your-company.com
  api_key: ek_your-ingest-key

interval: 60
lookback: 24

parsers:
  claude_code:
    enabled: true
  cursor:
    enabled: true
  copilot:
    enabled: true
  continuedev:
    enabled: true
EOF
```

### 3. Install as daemon

```bash
sudo ./install/install.sh
```

This installs the binary to `/usr/local/bin/eam-collector` and sets up:
- **macOS**: LaunchAgent (`~/Library/LaunchAgents/com.eam.collector.plist`)
- **Linux**: systemd service (`/etc/systemd/system/eam-collector.service`)

### 4. Verify

```bash
# Check if running
pgrep -f eam-collector

# Check logs
# macOS:
cat /tmp/eam-collector.log
# Linux:
journalctl -u eam-collector -f
```

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
3. **Detect** Claude account identity (org UUID from statsig cache + Desktop session paths)
4. **Send** records to EAM server `POST /api/ingest` with device ID + identities
5. **Save** state (file offsets, timestamps) only after successful send

### Lookback Filter

The `lookback` setting limits which files are processed. Only sessions modified within the lookback window are read. Set to `2` for "active sessions only" mode — no historical backlog on first run.

### Identity Detection

The collector extracts Claude organization UUIDs from two sources:
- Statsig cache: `~/.claude/statsig/statsig.cached.evaluations*`
- Desktop session paths: `~/Library/Application Support/Claude/local-agent-mode-sessions/{account}/{org}/`

These are sent as `identities[]` in the ingest payload. The EAM server checks if any org UUID matches `GOVERNED_ORG_IDS` to determine governance.

### State Persistence

State is saved to `~/.eam-collector/state.json` (mode 600) **only after successful send**. If the server is unreachable, state is not updated — records will be re-sent on the next cycle.

Per-parser state:
- **Claude**: byte offsets per JSONL file
- **Cursor**: last processed `createdAt` timestamp
- **Copilot**: file modification times
- **Continue.dev**: set of processed session IDs

## Uninstall

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
go build -o dist/eam-collector ./cmd/eam-collector/   # build
go test ./...                                           # test
./dist/eam-collector -config config.example.yaml        # run locally
```

## License

Proprietary - Autobahn Security
