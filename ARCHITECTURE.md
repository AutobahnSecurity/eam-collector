# eam-collector Architecture

## Overview

```
  DEVELOPER MACHINE
  ┌──────────────────────────────────────────────────────────┐
  │                                                          │
  │  AI Tools (write local session data)                     │
  │  ├── Claude Code    → ~/.claude/projects/*.jsonl         │
  │  ├── Cursor         → state.vscdb (SQLite)               │
  │  ├── GitHub Copilot → workspaceStorage/chatSessions/     │
  │  └── Continue.dev   → ~/.continue/sessions/              │
  │                                                          │
  │  eam-collector daemon                                    │
  │  ┌────────────────────────────────────────────────────┐  │
  │  │                                                    │  │
  │  │  ┌──────────┐ ┌──────────┐ ┌──────────┐          │  │
  │  │  │ Claude   │ │ Cursor   │ │ Copilot  │  ...      │  │
  │  │  │ Parser   │ │ Parser   │ │ Parser   │           │  │
  │  │  └────┬─────┘ └────┬─────┘ └────┬─────┘          │  │
  │  │       │             │             │                │  │
  │  │       ▼             ▼             ▼                │  │
  │  │  ┌──────────────────────────────────────┐         │  │
  │  │  │  Unified Record format               │         │  │
  │  │  │  {source, session_id, role, content,  │         │  │
  │  │  │   model, tokens, cost, ai_vendor}     │         │  │
  │  │  └────────────────┬─────────────────────┘         │  │
  │  │                   │                                │  │
  │  │  ┌────────────────▼─────────────────────┐         │  │
  │  │  │  Identity Detection                  │         │  │
  │  │  │  Claude org UUIDs from:              │         │  │
  │  │  │  - statsig cache                     │         │  │
  │  │  │  - Desktop session paths             │         │  │
  │  │  └────────────────┬─────────────────────┘         │  │
  │  │                   │                                │  │
  │  │  ┌────────────────▼─────────────────────┐         │  │
  │  │  │  Sender (HTTP POST)                  │         │  │
  │  │  │  → POST /api/ingest                  │         │  │
  │  │  │  → Header: X-EAM-Key: ek_***         │         │  │
  │  │  │  → Batch: 500 records max            │         │  │
  │  │  │  → Retry: 3 attempts, exp backoff    │         │  │
  │  │  └────────────────┬─────────────────────┘         │  │
  │  │                   │                                │  │
  │  │  ┌────────────────▼─────────────────────┐         │  │
  │  │  │  State Store                         │         │  │
  │  │  │  ~/.eam-collector/state.json         │         │  │
  │  │  │  Saved ONLY after successful send    │         │  │
  │  │  └──────────────────────────────────────┘         │  │
  │  └────────────────────────────────────────────────────┘  │
  └──────────────────────────────────────────────────────────┘
                          │
                          │ HTTPS + ek_* API key
                          ▼
  EAM SERVER
  ┌──────────────────────────────────────────────────────────┐
  │  POST /api/ingest                                        │
  │  ├── Validate API key (ingest_keys table)                │
  │  ├── Check identities vs GOVERNED_ORG_IDS (per-source)   │
  │  ├── Resolve user email from MDE device registry         │
  │  ├── Run DLP scan on prompt content                      │
  │  ├── Store in ai_usage + prompt_log                      │
  │  └── Return {stored, prompts, flagged, governed_sources}  │
  └──────────────────────────────────────────────────────────┘
```

## Project Structure

```
eam-collector/
├── cmd/eam-collector/
│   └── main.go              # Entry point, polling loop, signal handling
├── internal/
│   ├── config/
│   │   └── config.go         # YAML config loading + validation
│   ├── parsers/
│   │   ├── types.go           # Record, Parser interface, identity detection
│   │   ├── claude.go          # Claude Code JSONL parser
│   │   ├── cursor.go          # Cursor SQLite parser
│   │   ├── copilot.go         # GitHub Copilot JSON parser
│   │   └── continuedev.go     # Continue.dev JSON parser
│   ├── sender/
│   │   └── sender.go          # HTTP client, batching, retry logic
│   └── state/
│       └── state.go           # Persistent state (file offsets, timestamps)
├── install/
│   ├── install.sh             # Installer script
│   ├── com.eam.collector.plist  # macOS LaunchAgent
│   └── eam-collector.service    # Linux systemd unit
├── config.example.yaml
├── Makefile
├── go.mod
└── go.sum
```

## Parser Details

### Claude Code (`claude.go`)

- **Source**: JSONL files in `~/.claude/projects/`
- **State**: byte offset per file — resumes reading where it left off
- **Lookback**: skips files with `mtime` older than lookback window
- **Content**: extracts user prompts (string) and assistant responses (text blocks from array)
- **Session ID**: `collector:claude:{uuid}` from JSONL `sessionId` field
- **Filtering**: skips `<synthetic>` model entries, requires `type = "user"` or `"assistant"`

### Cursor (`cursor.go`)

- **Source**: SQLite database (`state.vscdb`) at platform-specific path
- **State**: `last_processed_ts` — Unix millisecond timestamp of newest processed composer
- **Lookback**: skips composers with `createdAt` older than lookback window
- **Formats**: handles both old (bubbleId) and new (inline conversation) Cursor versions
- **Role mapping**: `type=1` → user, `type=2` → assistant
- **Bubbles**: old-format entries only processed on subsequent runs (no timestamps for lookback)

### Copilot (`copilot.go`)

- **Source**: JSON files in VS Code `workspaceStorage/*/chatSessions/`
- **State**: file modification times (`mtime`) — skips unchanged files
- **Structure**: session → requests[] → {message, response[]}

### Continue.dev (`continuedev.go`)

- **Source**: JSON files in `~/.continue/sessions/`
- **State**: set of processed session IDs — one-pass read per session
- **Content**: handles both string and array content formats

## Governance Flow

The collector does NOT determine governance itself. It sends identities, and the server decides.

```
1. Collector reads Claude statsig cache + Desktop session paths
   → Extracts all (accountUUID, organizationUUID) pairs
   
2. Sends to server as identities[]:
   [
     {account: "ff8b...", org: "4ebb...", tool: "claude-desktop"},
     {account: "ff8b...", org: "d02f...", tool: "claude-code"},
   ]

3. Server checks each identity:
   → org "4ebb..." in GOVERNED_ORG_IDS? Yes → claude-desktop records governed
   → org "d02f..." in GOVERNED_ORG_IDS? No  → claude-code records shadow

4. Governance is per-source, NOT per-payload:
   → Claude records: governed (matching org)
   → Cursor records in same payload: shadow (no Cursor identity)
```

## Reliability

### Incremental Collection

Each parser tracks its position and only reads new data:
- Claude: file byte offsets (seek to last position)
- Cursor: `createdAt` timestamp (skip older composers)
- Copilot: file mtime (skip unchanged files)
- Continue: processed session ID set (skip known sessions)

### At-Least-Once Delivery

State is saved **only after** the server responds 200. If the send fails:
- State is NOT updated
- Next cycle re-reads from the last successful position
- Duplicate records are handled by the server's upsert logic

### Graceful Shutdown

SIGTERM/SIGINT handlers save state before exit. No data is lost during restarts or upgrades.

### Error Isolation

Each parser runs independently. If Cursor parsing fails, Claude and Copilot still proceed. Errors are logged but don't stop the collection cycle.

## Cross-Platform Support

| Platform | Binary | Daemon | Config path |
|----------|--------|--------|------------|
| macOS (Apple Silicon) | `eam-collector-darwin-arm64` | LaunchAgent | `~/.eam-collector/config.yaml` |
| macOS (Intel) | `eam-collector-darwin-amd64` | LaunchAgent | `~/.eam-collector/config.yaml` |
| Linux (x86_64) | `eam-collector-linux-amd64` | systemd | `~/.eam-collector/config.yaml` |
| Windows (x86_64) | `eam-collector-windows-amd64.exe` | (manual) | `%USERPROFILE%\.eam-collector\config.yaml` |

### Tool Data Paths

| Tool | macOS | Linux | Windows |
|------|-------|-------|---------|
| Claude Code | `~/.claude/projects/` | `~/.claude/projects/` | `%USERPROFILE%\.claude\projects\` |
| Cursor | `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` | `~/.config/Cursor/User/globalStorage/state.vscdb` | `%APPDATA%\Cursor\User\globalStorage\state.vscdb` |
| Copilot | `~/Library/Application Support/Code/User/workspaceStorage/` | `~/.config/Code/User/workspaceStorage/` | `%APPDATA%\Code\User\workspaceStorage\` |
| Continue.dev | `~/.continue/sessions/` | `~/.continue/sessions/` | `%USERPROFILE%\.continue\sessions\` |
