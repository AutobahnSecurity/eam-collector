package parsers

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
)

// ── Unified Claude Parser ──────────────────────────────────────────────
//
// Handles all Claude surfaces: Desktop (chat, code, cowork) and standalone CLI.
// Uses Desktop session metadata (lastActivityAt) as the primary signal for
// active sessions, with JSONL mtime as fallback for standalone CLI.
//
// Implementation is split across files:
//   claude.go          — orchestrator: Collect, session discovery
//   claude_jsonl.go    — JSONL parsing, content extraction
//   claude_chat.go     — chat mode (tipTap/IndexedDB), model detection
//   claude_helpers.go  — state restoration, file resolution, metadata

type ClaudeParser struct {
	projectsDir string // ~/.claude/projects/
	desktopDir  string // ~/Library/Application Support/Claude/
	lookback    time.Duration
}

func NewClaudeParser() *ClaudeParser {
	home, err := platform.HomeDir()
	if err != nil {
		log.Printf("[claude] Warning: %v", err)
		home = ""
	}
	return &ClaudeParser{
		projectsDir: filepath.Join(home, ".claude", "projects"),
		desktopDir:  platform.ClaudeDesktopDir(home),
		lookback:    24 * time.Hour,
	}
}

func (p *ClaudeParser) Name() string   { return "claude" }
func (p *ClaudeParser) DataDir() string { return p.projectsDir }

func (p *ClaudeParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *ClaudeParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	offsets := restoreOffsets(prevState)
	knownFiles := restoreKnownFiles(prevState)

	var records []Record
	newOffsets := make(map[string]float64)
	newKnown := make(map[string]bool)

	// ── Path 1: Code/Cowork sessions (JSONL files) ──
	sessions := p.findActiveSessions()

	for _, sess := range sessions {
		path := sess.DataPath
		newKnown[path] = true

		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		if !knownFiles[path] {
			// New file: baseline to current size, emit nothing
			newOffsets[path] = float64(info.Size())
			log.Printf("[claude] Baselined %s (%d bytes)", filepath.Base(path), info.Size())
			continue
		}

		// Known file: read incrementally from stored offset
		prevOffset := int64(offsets[path])
		if info.Size() <= prevOffset {
			// No new data
			newOffsets[path] = float64(prevOffset)
			continue
		}

		recs, newOffset, err := parseJSONL(path, prevOffset)
		if err != nil {
			log.Printf("[claude] Error parsing %s: %v", filepath.Base(path), err)
			newOffsets[path] = float64(prevOffset)
			continue
		}

		// Tag records with source and identity
		for i := range recs {
			recs[i].Source = sess.Source
			recs[i].AIVendor = "Anthropic"
			recs[i].Identity = sess.Identity
		}

		records = append(records, recs...)
		newOffsets[path] = float64(newOffset)
	}

	// Carry forward known files not active this cycle (prevents re-baselining)
	for path := range knownFiles {
		if !newKnown[path] {
			newKnown[path] = true
			if off, ok := offsets[path]; ok {
				newOffsets[path] = off
			}
		}
	}

	// ── Path 2: Chat mode (tipTap editor snapshots in IndexedDB) ──
	chatRecs, chatState := p.collectChat(prevState)
	records = append(records, chatRecs...)

	newState := map[string]any{
		"file_offsets": newOffsets,
		"known_files":  newKnown,
	}
	// Merge chat state
	for k, v := range chatState {
		newState[k] = v
	}
	return records, newState, nil
}

// ── Active Session Detection ───────────────────────────────────────────

func (p *ClaudeParser) findActiveSessions() []activeSession {
	cutoff := time.Now().Add(-p.lookback)

	// Phase 1: Desktop sessions (primary — uses lastActivityAt from metadata)
	desktopSessions, claimedPaths := p.scanDesktopSessions(cutoff)

	// Phase 2: Standalone CLI (fallback — JSONL mtime for non-Desktop sessions)
	cliSessions := p.scanStandaloneCLI(cutoff, claimedPaths)

	return append(desktopSessions, cliSessions...)
}

// scanDesktopSessions reads Desktop session metadata to find active sessions.
// Returns sessions and a set of JSONL paths claimed by Desktop (for dedup).
func (p *ClaudeParser) scanDesktopSessions(cutoff time.Time) ([]activeSession, map[string]bool) {
	claimed := make(map[string]bool)
	var sessions []activeSession

	for _, baseDir := range []string{
		filepath.Join(p.desktopDir, "claude-code-sessions"),
		filepath.Join(p.desktopDir, "local-agent-mode-sessions"),
	} {
		accounts, err := os.ReadDir(baseDir)
		if err != nil {
			continue
		}

		for _, acctEntry := range accounts {
			if !acctEntry.IsDir() {
				continue
			}
			acctName := acctEntry.Name()
			if acctName == "skills-plugin" || !uuidRe.MatchString(acctName) {
				continue
			}

			orgs, err := os.ReadDir(filepath.Join(baseDir, acctName))
			if err != nil {
				continue
			}

			for _, orgEntry := range orgs {
				if !orgEntry.IsDir() || !uuidRe.MatchString(orgEntry.Name()) {
					continue
				}

				orgDir := filepath.Join(baseDir, acctName, orgEntry.Name())
				found := p.findDesktopSessionsInDir(orgDir, acctName, orgEntry.Name(), cutoff)

				for _, sess := range found {
					claimed[sess.DataPath] = true
					sessions = append(sessions, sess)
				}
			}
		}
	}

	return sessions, claimed
}

func (p *ClaudeParser) findDesktopSessionsInDir(orgDir, accountUUID, orgUUID string, cutoff time.Time) []activeSession {
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		return nil
	}

	var sessions []activeSession

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		meta, err := readDesktopMeta(filepath.Join(orgDir, name))
		if err != nil || meta.IsArchived {
			continue
		}

		// Check lookback window
		if meta.LastActivityAt > 0 {
			if time.UnixMilli(meta.LastActivityAt).Before(cutoff) {
				continue
			}
		} else {
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
		}

		// Resolve cliSessionId → JSONL path
		if meta.CLISessionID == "" {
			continue
		}
		dataPath := resolveClaudeProjectJSONL(meta.CLISessionID, meta.OriginCWD, meta.CWD, p.projectsDir)
		if dataPath == "" {
			continue
		}
		if _, err := os.Stat(dataPath); os.IsNotExist(err) {
			continue
		}

		sessions = append(sessions, activeSession{
			DataPath: dataPath,
			Source:   "claude-desktop",
			Identity: &AccountIdentity{
				AccountUUID:      accountUUID,
				OrganizationUUID: orgUUID,
				Tool:             "claude-desktop",
			},
		})
	}

	return sessions
}

// scanStandaloneCLI finds active CLI sessions not claimed by Desktop.
func (p *ClaudeParser) scanStandaloneCLI(cutoff time.Time, claimed map[string]bool) []activeSession {
	files, err := findJSONLFiles(p.projectsDir, p.lookback)
	if err != nil {
		return nil
	}

	// Get identity from statsig cache for standalone CLI sessions
	home, err := platform.HomeDir()
	if err != nil {
		log.Printf("[claude] Warning: %v", err)
		return nil
	}
	identity := readStatsigIdentity(home)
	if identity != nil {
		identity.Tool = "claude-code"
	}

	var sessions []activeSession
	for _, path := range files {
		if claimed[path] {
			continue
		}

		sessions = append(sessions, activeSession{
			DataPath: path,
			Source:   "claude-code",
			Identity: identity,
		})
	}

	return sessions
}
