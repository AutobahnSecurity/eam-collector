package parsers

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ClaudeDesktopParser reads Claude Desktop code/cowork sessions.
//
// Claude Desktop stores session data in two locations:
//
// 1. Legacy: local-agent-mode-sessions/{account}/{org}/local_{uuid}/audit.jsonl
//    Older sessions with full audit logs alongside the metadata.
//
// 2. Current: claude-code-sessions/{account}/{org}/local_{uuid}.json
//    Newer sessions store metadata here but conversation data is written to
//    ~/.claude/projects/{cwd-hash}/{cliSessionId}.jsonl (same location as CLI).
//    The cliSessionId in the metadata maps to the JSONL filename.
//
// Chat-only sessions (no audit.jsonl or cliSessionId) are skipped.
type ClaudeDesktopParser struct {
	appDir   string // ~/Library/Application Support/Claude/
	lookback time.Duration
}

func NewClaudeDesktopParser() *ClaudeDesktopParser {
	return &ClaudeDesktopParser{
		appDir:   claudeDesktopAppDir(),
		lookback: 24 * time.Hour,
	}
}

func (p *ClaudeDesktopParser) Name() string   { return "claude_desktop" }
func (p *ClaudeDesktopParser) DataDir() string { return p.appDir }

func (p *ClaudeDesktopParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

// desktopSessionMeta holds the fields we need from local_{uuid}.json.
type desktopSessionMeta struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	CWD            string `json:"cwd"`
	OriginCWD      string `json:"originCwd"`
	LastActivityAt int64  `json:"lastActivityAt"` // unix ms
	Model          string `json:"model"`
	IsArchived     bool   `json:"isArchived"`
}

type desktopSession struct {
	Meta        desktopSessionMeta
	DataPath    string // path to JSONL data (audit.jsonl or projects/*.jsonl)
	AccountUUID string
	OrgUUID     string
}

func (p *ClaudeDesktopParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	offsets := restoreOffsets(prevState)

	// Scan both legacy and current session directories
	sessions, err := p.findActiveSessions()
	if err != nil {
		return nil, prevState, fmt.Errorf("scan desktop sessions: %w", err)
	}

	if len(sessions) == 0 {
		newState["file_offsets"] = offsets
		return nil, newState, nil
	}

	var records []Record
	newOffsets := make(map[string]float64)

	for _, sess := range sessions {
		prevOffset := int64(offsets[sess.DataPath])

		recs, newOffset, err := ParseClaudeJSONLFile(
			sess.DataPath, prevOffset,
			"claude-desktop",
			fmt.Sprintf("collector:claude-desktop:%s", sess.Meta.SessionID),
			"Anthropic",
		)
		if err != nil {
			log.Printf("[claude_desktop] Error parsing %s: %v", sess.DataPath, err)
			newOffsets[sess.DataPath] = float64(prevOffset)
			continue
		}

		// Inject identity from directory path and model from metadata
		identity := &AccountIdentity{
			AccountUUID:      sess.AccountUUID,
			OrganizationUUID: sess.OrgUUID,
			Tool:             "claude-desktop",
		}
		for i := range recs {
			recs[i].Identity = identity
			if sess.Meta.Model != "" && recs[i].Model == "" {
				recs[i].Model = sess.Meta.Model
			}
		}

		records = append(records, recs...)
		newOffsets[sess.DataPath] = float64(newOffset)
	}

	newState["file_offsets"] = newOffsets
	return records, newState, nil
}

// DesktopSessionCLIIDs returns the set of cliSessionId values for active Desktop
// sessions. The Claude Code parser uses this to skip JSONL files that belong to
// Desktop (avoiding double-counting).
func (p *ClaudeDesktopParser) DesktopSessionCLIIDs() map[string]bool {
	sessions, _ := p.findActiveSessions()
	ids := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		if s.Meta.CLISessionID != "" {
			ids[s.Meta.CLISessionID] = true
		}
	}
	return ids
}

func (p *ClaudeDesktopParser) findActiveSessions() ([]desktopSession, error) {
	cutoff := time.Now().Add(-p.lookback)
	var sessions []desktopSession

	// Scan legacy: local-agent-mode-sessions/{account}/{org}/
	legacyDir := filepath.Join(p.appDir, "local-agent-mode-sessions")
	if s, err := p.scanSessionDir(legacyDir, cutoff, true); err == nil {
		sessions = append(sessions, s...)
	}

	// Scan current: claude-code-sessions/{account}/{org}/
	currentDir := filepath.Join(p.appDir, "claude-code-sessions")
	if s, err := p.scanSessionDir(currentDir, cutoff, false); err == nil {
		sessions = append(sessions, s...)
	}

	return sessions, nil
}

// scanSessionDir walks a {account}/{org}/ directory tree for session metadata.
// If legacy=true, looks for audit.jsonl in a subdirectory.
// If legacy=false, resolves cliSessionId to a ~/.claude/projects/ JSONL file.
func (p *ClaudeDesktopParser) scanSessionDir(baseDir string, cutoff time.Time, legacy bool) ([]desktopSession, error) {
	accounts, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, nil // directory doesn't exist
	}

	var sessions []desktopSession

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
			if !orgEntry.IsDir() {
				continue
			}
			orgName := orgEntry.Name()
			if !uuidRe.MatchString(orgName) {
				continue
			}

			orgDir := filepath.Join(baseDir, acctName, orgName)
			found, err := p.findSessionsInDir(orgDir, acctName, orgName, cutoff, legacy)
			if err != nil {
				log.Printf("[claude_desktop] Error scanning %s: %v", orgDir, err)
				continue
			}
			sessions = append(sessions, found...)
		}
	}

	return sessions, nil
}

func (p *ClaudeDesktopParser) findSessionsInDir(orgDir, accountUUID, orgUUID string, cutoff time.Time, legacy bool) ([]desktopSession, error) {
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		return nil, err
	}

	var sessions []desktopSession

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		metaPath := filepath.Join(orgDir, name)
		meta, err := readDesktopMeta(metaPath)
		if err != nil {
			continue
		}

		if meta.IsArchived {
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

		// Resolve the JSONL data path
		var dataPath string
		if legacy {
			// Legacy: audit.jsonl in a subdirectory alongside the metadata
			sessionDirName := strings.TrimSuffix(name, ".json")
			dataPath = filepath.Join(orgDir, sessionDirName, "audit.jsonl")
		} else {
			// Current: cliSessionId maps to ~/.claude/projects/{cwd-hash}/{id}.jsonl
			if meta.CLISessionID == "" {
				continue
			}
			dataPath = resolveClaudeProjectJSONL(meta.CLISessionID, meta.OriginCWD, meta.CWD)
		}

		if dataPath == "" {
			continue
		}
		if _, err := os.Stat(dataPath); os.IsNotExist(err) {
			continue
		}

		sessions = append(sessions, desktopSession{
			Meta:        *meta,
			DataPath:    dataPath,
			AccountUUID: accountUUID,
			OrgUUID:     orgUUID,
		})
	}

	return sessions, nil
}

// resolveClaudeProjectJSONL finds the JSONL file for a cliSessionId in ~/.claude/projects/.
// Claude Code stores session files at ~/.claude/projects/{cwd-hash}/{sessionId}.jsonl
// where cwd-hash is derived from the working directory path.
func resolveClaudeProjectJSONL(cliSessionID, originCWD, cwd string) string {
	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	filename := cliSessionID + ".jsonl"

	// Try to derive the cwd-hash directory name from the working directory.
	// Claude Code converts "/" to "-" in the path to create the directory name.
	for _, dir := range []string{originCWD, cwd} {
		if dir == "" {
			continue
		}
		// Claude Code uses the path with / replaced by - as the directory name
		cwdHash := strings.ReplaceAll(dir, "/", "-")
		candidate := filepath.Join(projectsDir, cwdHash, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fallback: search all project directories for the session file
	dirs, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, d.Name(), filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return ""
}

func readDesktopMeta(path string) (*desktopSessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta desktopSessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func claudeDesktopAppDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude")
	default:
		return filepath.Join(home, ".config", "Claude")
	}
}
