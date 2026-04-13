package parsers

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ClaudeLine represents a single JSONL line from Claude Code / Desktop audit files.
type ClaudeLine struct {
	Type      string `json:"type"` // "user", "assistant", "system", etc.
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	UUID      string `json:"uuid"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // string for user, array for assistant
		Model   string          `json:"model"`
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	AuditTimestamp string `json:"_audit_timestamp"`
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

// activeSession represents a Claude session that is currently active.
type activeSession struct {
	DataPath string           // path to JSONL file
	Source   string           // "claude-desktop" or "claude-code"
	Identity *AccountIdentity // from Desktop dir path or statsig
}

// ── Unified Claude Parser ──────────────────────────────────────────────
//
// Handles all Claude surfaces: Desktop (chat, code, cowork) and standalone CLI.
// Uses Desktop session metadata (lastActivityAt) as the primary signal for
// active sessions, with JSONL mtime as fallback for standalone CLI.

type ClaudeParser struct {
	projectsDir string // ~/.claude/projects/
	desktopDir  string // ~/Library/Application Support/Claude/
	lookback    time.Duration
}

func NewClaudeParser() *ClaudeParser {
	home, _ := os.UserHomeDir()
	return &ClaudeParser{
		projectsDir: filepath.Join(home, ".claude", "projects"),
		desktopDir:  claudeDesktopAppDir(),
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

	sessions := p.findActiveSessions()
	if len(sessions) == 0 {
		// Nothing active — carry forward state, no records
		return nil, prevState, nil
	}

	var records []Record
	newOffsets := make(map[string]float64)
	newKnown := make(map[string]bool)

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

	newState := map[string]any{
		"file_offsets": newOffsets,
		"known_files":  newKnown,
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
	home, _ := os.UserHomeDir()
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

// ── JSONL Parsing ──────────────────────────────────────────────────────

// parseJSONL reads a Claude JSONL file from the given byte offset.
// Returns user/assistant records and the new file offset.
func parseJSONL(path string, offset int64) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var cl ClaudeLine
		if err := json.Unmarshal(line, &cl); err != nil {
			continue
		}

		if cl.Type != "user" && cl.Type != "assistant" {
			continue
		}

		content := ExtractContent(cl.Message.Content)
		if content == "" {
			continue
		}

		ts := cl.Timestamp
		if ts == "" {
			ts = cl.AuditTimestamp
		}
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}

		sessionID := "collector:claude:" + cl.SessionID

		model := cl.Message.Model
		if model == "<synthetic>" {
			model = ""
		}

		records = append(records, Record{
			SessionID:    sessionID,
			Timestamp:    ts,
			Role:         cl.Message.Role,
			Content:      content,
			Model:        model,
			InputTokens:  cl.Message.Usage.InputTokens,
			OutputTokens: cl.Message.Usage.OutputTokens,
		})
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	return records, newOffset, scanner.Err()
}

// ── Helpers ────────────────────────────────────────────────────────────

// ExtractContent handles both string (user) and array (assistant) content formats.
func ExtractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

func restoreOffsets(prevState map[string]any) map[string]float64 {
	offsets := make(map[string]float64)
	if raw, ok := prevState["file_offsets"]; ok {
		switch m := raw.(type) {
		case map[string]any:
			for k, v := range m {
				if f, ok := v.(float64); ok {
					offsets[k] = f
				}
			}
		case map[string]float64:
			for k, v := range m {
				offsets[k] = v
			}
		}
	}
	return offsets
}

func restoreKnownFiles(prevState map[string]any) map[string]bool {
	known := make(map[string]bool)
	if raw, ok := prevState["known_files"]; ok {
		switch m := raw.(type) {
		case map[string]any:
			for k, v := range m {
				if b, ok := v.(bool); ok && b {
					known[k] = true
				}
			}
		case map[string]bool:
			for k, v := range m {
				if v {
					known[k] = true
				}
			}
		}
	}
	return known
}

func findJSONLFiles(baseDir string, lookback time.Duration) ([]string, error) {
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			if time.Since(info.ModTime()) < lookback {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}

func resolveClaudeProjectJSONL(cliSessionID, originCWD, cwd, projectsDir string) string {
	filename := cliSessionID + ".jsonl"

	// Try to derive the cwd-hash directory from the working directory.
	// Claude Code converts "/" to "-" in the path to create the directory name.
	for _, dir := range []string{originCWD, cwd} {
		if dir == "" {
			continue
		}
		cwdHash := strings.ReplaceAll(dir, "/", "-")
		candidate := filepath.Join(projectsDir, cwdHash, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fallback: search all project directories
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
