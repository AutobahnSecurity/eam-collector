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

// ClaudeDesktopParser reads Claude Desktop code/cowork sessions from
// the local-agent-mode-sessions directory. Each session has a metadata
// JSON file and an audit.jsonl conversation log (same format as Claude Code).
//
// Chat-only sessions (no audit.jsonl) are skipped — they use IndexedDB
// binary format which is not parseable.
type ClaudeDesktopParser struct {
	sessionsDir string
	lookback    time.Duration
}

func NewClaudeDesktopParser() *ClaudeDesktopParser {
	return &ClaudeDesktopParser{
		sessionsDir: claudeDesktopSessionsPath(),
		lookback:    24 * time.Hour,
	}
}

func (p *ClaudeDesktopParser) Name() string   { return "claude_desktop" }
func (p *ClaudeDesktopParser) DataDir() string { return p.sessionsDir }

func (p *ClaudeDesktopParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

// desktopSessionMeta holds the fields we need from local_{uuid}.json.
type desktopSessionMeta struct {
	SessionID      string `json:"sessionId"`
	LastActivityAt int64  `json:"lastActivityAt"` // unix ms
	Model          string `json:"model"`
	IsArchived     bool   `json:"isArchived"`
}

type desktopSession struct {
	Meta       desktopSessionMeta
	AuditPath  string // full path to audit.jsonl
	AccountUUID string
	OrgUUID     string
}

func (p *ClaudeDesktopParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.sessionsDir); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	offsets := restoreOffsets(prevState)

	sessions, err := p.findActiveSessions()
	if err != nil {
		return nil, prevState, fmt.Errorf("scan desktop sessions: %w", err)
	}

	var records []Record
	newOffsets := make(map[string]float64)

	for _, sess := range sessions {
		prevOffset := int64(offsets[sess.AuditPath])

		recs, newOffset, err := ParseClaudeJSONLFile(
			sess.AuditPath, prevOffset,
			"claude-desktop",
			fmt.Sprintf("collector:claude-desktop:%s", sess.Meta.SessionID),
			"Anthropic",
		)
		if err != nil {
			log.Printf("[claude_desktop] Error parsing %s: %v", sess.AuditPath, err)
			newOffsets[sess.AuditPath] = float64(prevOffset)
			continue
		}

		// Inject model from metadata if not in JSONL lines
		if sess.Meta.Model != "" {
			for i := range recs {
				if recs[i].Model == "" {
					recs[i].Model = sess.Meta.Model
				}
			}
		}

		records = append(records, recs...)
		newOffsets[sess.AuditPath] = float64(newOffset)
	}

	newState["file_offsets"] = newOffsets
	return records, newState, nil
}

// findActiveSessions walks local-agent-mode-sessions/{account}/{org}/
// and returns sessions whose lastActivityAt falls within the lookback window.
func (p *ClaudeDesktopParser) findActiveSessions() ([]desktopSession, error) {
	accounts, err := os.ReadDir(p.sessionsDir)
	if err != nil {
		return nil, nil
	}

	cutoff := time.Now().Add(-p.lookback)
	var sessions []desktopSession

	for _, acctEntry := range accounts {
		if !acctEntry.IsDir() {
			continue
		}
		acctName := acctEntry.Name()
		if acctName == "skills-plugin" || !uuidRe.MatchString(acctName) {
			continue
		}

		orgs, err := os.ReadDir(filepath.Join(p.sessionsDir, acctName))
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

			orgDir := filepath.Join(p.sessionsDir, acctName, orgName)
			sess, err := p.findSessionsInOrgDir(orgDir, acctName, orgName, cutoff)
			if err != nil {
				log.Printf("[claude_desktop] Error scanning %s: %v", orgDir, err)
				continue
			}
			sessions = append(sessions, sess...)
		}
	}

	return sessions, nil
}

// findSessionsInOrgDir scans an org directory for active sessions with audit.jsonl files.
func (p *ClaudeDesktopParser) findSessionsInOrgDir(orgDir, accountUUID, orgUUID string, cutoff time.Time) ([]desktopSession, error) {
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		return nil, err
	}

	var sessions []desktopSession

	for _, entry := range entries {
		name := entry.Name()
		// Session metadata files: local_{uuid}.json (not directories, not .jsonl)
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

		// Check lookback window using lastActivityAt from metadata
		if meta.LastActivityAt > 0 {
			lastActivity := time.UnixMilli(meta.LastActivityAt)
			if lastActivity.Before(cutoff) {
				continue
			}
		} else {
			// Fall back to metadata file mtime
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
		}

		// Check for audit.jsonl in the session directory
		sessionDirName := strings.TrimSuffix(name, ".json")
		auditPath := filepath.Join(orgDir, sessionDirName, "audit.jsonl")
		if _, err := os.Stat(auditPath); os.IsNotExist(err) {
			continue // chat-only session (no audit log)
		}

		sessions = append(sessions, desktopSession{
			Meta:        *meta,
			AuditPath:   auditPath,
			AccountUUID: accountUUID,
			OrgUUID:     orgUUID,
		})
	}

	return sessions, nil
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

func claudeDesktopSessionsPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude", "local-agent-mode-sessions")
	default:
		return filepath.Join(home, ".config", "Claude", "local-agent-mode-sessions")
	}
}
