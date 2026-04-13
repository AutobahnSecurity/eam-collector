package parsers

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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
	var records []Record
	newOffsets := make(map[string]float64)

	// ── Path 1: Code/Cowork sessions (JSONL files) ──
	sessions, err := p.findActiveSessions()
	if err != nil {
		log.Printf("[claude_desktop] Error scanning sessions: %v", err)
	}

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

	// ── Path 2: Chat mode (tipTap editor state in IndexedDB WAL) ──
	chatRecs, chatState := p.collectChat(prevState)
	// Detect chat model from Local Storage
	if len(chatRecs) > 0 {
		if chatModel := p.detectChatModel(); chatModel != "" {
			for i := range chatRecs {
				if chatRecs[i].Model == "" {
					chatRecs[i].Model = chatModel
				}
			}
		}
	}
	records = append(records, chatRecs...)
	// Merge chat state into newState
	for k, v := range chatState {
		newState[k] = v
	}

	newState["file_offsets"] = newOffsets
	return records, newState, nil
}

// collectChat reads user messages from Claude Desktop chat mode.
// Chat mode stores editor drafts as tipTapEditorState JSON in the IndexedDB WAL log.
// As the user types, snapshots grow in length. When the text gets significantly shorter,
// the user submitted the previous message and started a new one.
func (p *ClaudeDesktopParser) collectChat(prevState map[string]any) ([]Record, map[string]any) {
	state := make(map[string]any)

	idbDir := filepath.Join(p.appDir, "IndexedDB", "https_claude.ai_0.indexeddb.leveldb")
	logPath := findNewestLog(idbDir)
	if logPath == "" {
		return nil, state
	}

	info, err := os.Stat(logPath)
	if err != nil || time.Since(info.ModTime()) > p.lookback {
		return nil, state
	}

	// Restore previous chat state
	var prevOffset float64
	if v, ok := prevState["chat_log_offset"]; ok {
		if f, ok := v.(float64); ok {
			prevOffset = f
		}
	}
	var prevLogName string
	if v, ok := prevState["chat_log_name"]; ok {
		if s, ok := v.(string); ok {
			prevLogName = s
		}
	}
	var lastTS float64
	if v, ok := prevState["chat_last_ts"]; ok {
		if f, ok := v.(float64); ok {
			lastTS = f
		}
	}

	// Detect WAL rotation — reset offset if log file changed
	currentLogName := filepath.Base(logPath)
	if prevLogName != "" && currentLogName != prevLogName {
		prevOffset = 0
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, state
	}

	startOffset := int(prevOffset)
	if startOffset > len(data) {
		startOffset = 0 // log was rotated/truncated
	}

	// First encounter: skip to end
	if startOffset == 0 && prevLogName == "" {
		state["chat_log_offset"] = float64(len(data))
		state["chat_log_name"] = currentLogName
		state["chat_last_ts"] = lastTS
		return nil, state
	}

	text := string(data[startOffset:])
	snapshots := extractSnapshots(text)

	if len(snapshots) == 0 {
		state["chat_log_offset"] = float64(len(data))
		state["chat_log_name"] = currentLogName
		state["chat_last_ts"] = lastTS
		return nil, state
	}

	messages := extractSentMessages(snapshots, lastTS)

	var records []Record
	var maxTS float64

	for _, msg := range messages {
		if msg.Timestamp <= lastTS {
			continue
		}
		if time.Since(time.UnixMilli(int64(msg.Timestamp))) > p.lookback {
			continue
		}

		ts := time.UnixMilli(int64(msg.Timestamp)).UTC().Format(time.RFC3339)
		sessionID := fmt.Sprintf("collector:claude-desktop:chat-%d", int64(msg.Timestamp)/3600000)

		records = append(records, Record{
			Source:    "claude-desktop",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      "user",
			Content:   msg.Text,
			AIVendor:  "Anthropic",
		})

		if msg.Timestamp > maxTS {
			maxTS = msg.Timestamp
		}
	}

	if maxTS > lastTS {
		state["chat_last_ts"] = maxTS
	} else {
		state["chat_last_ts"] = lastTS
	}
	state["chat_log_offset"] = float64(len(data))
	state["chat_log_name"] = currentLogName

	return records, state
}

// detectChatModel reads the currently selected model from Claude Desktop's
// Local Storage (Chromium LevelDB). The model is stored as "api_model" in
// the React Query cache within the WAL log file.
func (p *ClaudeDesktopParser) detectChatModel() string {
	lsDir := filepath.Join(p.appDir, "Local Storage", "leveldb")
	entries, err := os.ReadDir(lsDir)
	if err != nil {
		return ""
	}

	// Check .log files first (active WAL has latest data), then .ldb
	var files []string
	for _, e := range entries {
		name := e.Name()
		if name == "LOG" || name == "LOG.old" || name == "LOCK" || name == "CURRENT" || e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".log") {
			files = append([]string{filepath.Join(lsDir, name)}, files...)
		} else if strings.HasSuffix(name, ".ldb") {
			files = append(files, filepath.Join(lsDir, name))
		}
	}

	modelRe := regexp.MustCompile(`api_model.{0,10}(claude-[a-z]+-[0-9a-z.-]+)`)

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// Search UTF-16LE decoded content (Chromium Local Storage uses UTF-16)
		decoded := decodeUTF16LE(data)
		if m := modelRe.FindStringSubmatch(decoded); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// decodeUTF16LE decodes byte data as UTF-16LE, ignoring errors.
func decodeUTF16LE(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	// Fast path: decode pairs of bytes as UTF-16LE runes
	var sb strings.Builder
	sb.Grow(len(data) / 2)
	for i := 0; i+1 < len(data); i += 2 {
		r := rune(data[i]) | rune(data[i+1])<<8
		if r == 0 {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// ── tipTap snapshot parsing ──

type tipTapSnapshot struct {
	State struct {
		TipTapEditorState struct {
			Content []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"tipTapEditorState"`
	} `json:"state"`
	UpdatedAt int64 `json:"updatedAt"` // Unix millis
}

type sentMessage struct {
	Text      string
	Timestamp float64
}

func extractSnapshots(text string) []tipTapSnapshot {
	var snapshots []tipTapSnapshot
	idx := 0
	for {
		start := strings.Index(text[idx:], `{"state":{"tipTapEditorState":`)
		if start == -1 {
			break
		}
		start += idx

		depth := 0
		end := -1
		for i := start; i < len(text) && i < start+10000; i++ {
			if text[i] == '{' {
				depth++
			} else if text[i] == '}' {
				depth--
				if depth == 0 {
					end = i + 1
					break
				}
			}
		}

		if end == -1 {
			idx = start + 1
			continue
		}

		var snap tipTapSnapshot
		if err := json.Unmarshal([]byte(text[start:end]), &snap); err == nil && snap.UpdatedAt > 0 {
			snapshots = append(snapshots, snap)
		}
		idx = end
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].UpdatedAt < snapshots[j].UpdatedAt
	})

	return snapshots
}

func extractSentMessages(snapshots []tipTapSnapshot, afterTS float64) []sentMessage {
	var messages []sentMessage
	var prevText string
	var prevTS float64

	for _, snap := range snapshots {
		text := ""
		for _, block := range snap.State.TipTapEditorState.Content {
			for _, inline := range block.Content {
				text += inline.Text
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		ts := float64(snap.UpdatedAt)

		// Detect message boundary: text got significantly shorter or time gap > 10s
		if prevText != "" && (len(text) < len(prevText)/2 || (ts-prevTS > 10000 && len(text) < len(prevText))) {
			if len(prevText) > 3 && prevTS > afterTS {
				messages = append(messages, sentMessage{Text: prevText, Timestamp: prevTS})
			}
		}

		prevText = text
		prevTS = ts
	}

	// Last message being typed
	if prevText != "" && len(prevText) > 3 && prevTS > afterTS {
		messages = append(messages, sentMessage{Text: prevText, Timestamp: prevTS})
	}

	return messages
}

// findNewestLog finds the most recently modified .log file in a LevelDB directory.
func findNewestLog(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMtime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if e.Name() == "LOG" || e.Name() == "LOG.old" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if mt := info.ModTime().UnixNano(); mt > bestMtime {
			bestMtime = mt
			best = filepath.Join(dir, e.Name())
		}
	}
	return best
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
