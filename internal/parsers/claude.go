package parsers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Parser-level constants
const (
	// scannerBufSize is the buffer size for JSONL line scanning.
	scannerBufSize = 1024 * 1024 // 1 MB

	// maxLDBFileSize caps LevelDB file reads to prevent excessive memory use.
	maxLDBFileSize = 10 * 1024 * 1024 // 10 MB

	// maxJSONLReadSize caps how much new JSONL data is read per cycle (50 MB).
	// Prevents unbounded memory use if a file grew massively between cycles.
	maxJSONLReadSize = 50 * 1024 * 1024

	// tipTapWindowSize is the byte window after a tipTap marker to extract context.
	tipTapWindowSize = 5000

	// tipTapPreWindow is the byte window before a tipTap marker.
	tipTapPreWindow = 50

	// minSubmissionLen is the minimum peak text length to consider as a submission.
	minSubmissionLen = 10

	// shortTextThreshold: text below this after a substantial peak signals submission.
	shortTextThreshold = 20

	// chatModelSearchWindow is bytes to search after a sticky-model-selector key.
	chatModelSearchWindow = 200
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
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[claude] Warning: cannot resolve home directory: %v", err)
		home = ""
	}
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
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[claude] Warning: cannot resolve home directory for CLI identity: %v", err)
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

// ── JSONL Parsing ──────────────────────────────────────────────────────

// parseJSONL reads a Claude JSONL file from the given byte offset.
// Returns user/assistant records and the new file offset.
//
// Reads all new data into memory and processes only complete lines (up to
// the last newline). Incomplete trailing lines are left for the next cycle.
func parseJSONL(path string, offset int64) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	// Read new data from the offset position, capped to prevent unbounded memory use
	newData, err := io.ReadAll(io.LimitReader(f, maxJSONLReadSize))
	if err != nil {
		return nil, offset, err
	}
	if len(newData) == 0 {
		return nil, offset, nil
	}

	// Only process complete lines (up to last newline).
	// Incomplete trailing lines are left for the next cycle.
	lastNL := bytes.LastIndexByte(newData, '\n')
	if lastNL == -1 {
		return nil, offset, nil // no complete lines yet
	}
	processable := newData[:lastNL+1]

	var records []Record
	for _, line := range bytes.Split(processable, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}

		var cl ClaudeLine
		if err := json.Unmarshal(line, &cl); err != nil {
			log.Printf("[claude] Skipping malformed JSONL line in %s: %v", filepath.Base(path), err)
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

	newOffset := offset + int64(len(processable))
	return records, newOffset, nil
}

// ── Chat Mode (tipTap snapshots from IndexedDB) ───────────────────────

// collectChat reads user messages from Claude Desktop chat mode.
// Chat mode stores tipTap editor snapshots in IndexedDB LevelDB.
// Each keystroke creates a snapshot; when the text shrinks significantly,
// the previous long text was the submitted message.
func (p *ClaudeParser) collectChat(prevState map[string]any) ([]Record, map[string]any) {
	state := make(map[string]any)

	idbDir := filepath.Join(p.desktopDir, "IndexedDB", "https_claude.ai_0.indexeddb.leveldb")
	if _, err := os.Stat(idbDir); os.IsNotExist(err) {
		return nil, state
	}

	// Restore previous chat state
	var lastTS float64
	if v, ok := prevState["chat_last_ts"]; ok {
		if f, ok := v.(float64); ok {
			lastTS = f
		}
	}
	isFirstRun := prevState == nil || prevState["chat_last_ts"] == nil

	// Read .ldb and .log files modified within lookback window.
	// LevelDB doesn't append linearly, so we read entire files and
	// rely on chat_last_ts to filter already-seen snapshots.
	entries, err := os.ReadDir(idbDir)
	if err != nil {
		return nil, state
	}

	var snapshots []tipTapSnapshot

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".ldb") && !strings.HasSuffix(name, ".log") {
			continue
		}

		path := filepath.Join(idbDir, name)
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) > p.lookback {
			continue
		}

		// Skip oversized files to prevent excessive memory use
		if info.Size() > maxLDBFileSize {
			log.Printf("[claude] Skipping oversized LevelDB file %s (%d bytes)", name, info.Size())
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		extracted := extractTipTapSnapshots(data)
		snapshots = append(snapshots, extracted...)
	}

	if len(snapshots) == 0 || isFirstRun {
		// First run: just record the current max timestamp
		maxTS := lastTS
		for _, s := range snapshots {
			if s.UpdatedAt > maxTS {
				maxTS = s.UpdatedAt
			}
		}
		if maxTS > lastTS {
			state["chat_last_ts"] = maxTS
		} else {
			state["chat_last_ts"] = lastTS
		}
		return nil, state
	}

	// Sort by timestamp and detect submissions
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].UpdatedAt < snapshots[j].UpdatedAt
	})
	messages := detectSubmissions(snapshots, lastTS)

	var records []Record
	var maxTS float64

	// Get identity for chat records — prefer Desktop directory path (governed account)
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[claude] Warning: cannot resolve home directory for chat identity: %v", err)
		return nil, state
	}
	identity := readDesktopIdentity(home)
	if identity == nil {
		identity = readStatsigIdentity(home)
	}
	if identity != nil {
		identity.Tool = "claude-desktop"
	}

	// Read the currently selected model from Desktop Local Storage
	chatModel := readChatModel(p.desktopDir)

	for _, msg := range messages {
		submitTS := msg.SubmittedAt
		if submitTS == 0 {
			submitTS = msg.UpdatedAt
		}
		ts := time.UnixMilli(int64(submitTS)).UTC().Format(time.RFC3339)
		sessionID := fmt.Sprintf("collector:claude:chat-%d", int64(submitTS)/3600000)

		rec := Record{
			Source:    "claude-desktop",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      "user",
			Content:   msg.Text,
			Model:     chatModel,
			AIVendor:  "Anthropic",
			Identity:  identity,
		}
		records = append(records, rec)

		if msg.UpdatedAt > maxTS {
			maxTS = msg.UpdatedAt
		}
	}

	if maxTS > lastTS {
		state["chat_last_ts"] = maxTS
	} else {
		state["chat_last_ts"] = lastTS
	}

	return records, state
}

type tipTapSnapshot struct {
	Text        string
	UpdatedAt   float64
	SubmittedAt float64 // set by detectSubmissions to the shrink timestamp
}

// extractTipTapSnapshots extracts tipTap editor snapshots from LevelDB binary data.
func extractTipTapSnapshots(data []byte) []tipTapSnapshot {
	needle := []byte("tipTapEditorS")
	var snapshots []tipTapSnapshot

	for i := 0; i < len(data)-len(needle); {
		// Use bytes.Index for O(n) search instead of byte-by-byte comparison
		idx := bytes.Index(data[i:], needle)
		if idx == -1 {
			break
		}
		i += idx // absolute position of this match

		// Extract a chunk around this position for parsing
		start := i - tipTapPreWindow
		if start < 0 {
			start = 0
		}
		end := i + tipTapWindowSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]

		// Strip non-printable bytes for text matching
		clean := make([]byte, 0, len(chunk))
		for _, b := range chunk {
			if b >= 32 && b < 127 {
				clean = append(clean, b)
			}
		}
		s := string(clean)

		// Extract updatedAt timestamp
		tsIdx := strings.Index(s, `"updatedAt":`)
		if tsIdx == -1 {
			i += len(needle)
			continue
		}
		tsStart := tsIdx + len(`"updatedAt":`)
		tsEnd := tsStart
		for tsEnd < len(s) && s[tsEnd] >= '0' && s[tsEnd] <= '9' {
			tsEnd++
		}
		if tsEnd == tsStart {
			i += len(needle)
			continue
		}
		ts := parseTimestamp(s[tsStart:tsEnd])
		if ts == 0 {
			i += len(needle)
			continue
		}

		// Extract text content
		text := extractTipTapText(s)

		snapshots = append(snapshots, tipTapSnapshot{
			Text:      text,
			UpdatedAt: ts,
		})

		i += len(needle)
	}

	return snapshots
}

// extractTipTapText extracts the message text from a cleaned tipTap snapshot string.
func extractTipTapText(s string) string {
	// Find the text field value — it appears as: text",...:"actual content here"}]}
	textIdx := strings.Index(s, `text",`)
	if textIdx == -1 {
		textIdx = strings.Index(s, `"text"`)
		if textIdx == -1 {
			return ""
		}
	}

	// Find the colon-quote that starts the value
	sub := s[textIdx:]
	valStart := -1
	for i := 0; i < len(sub)-1; i++ {
		if sub[i] == ':' && sub[i+1] == '"' {
			valStart = i + 2
			break
		}
	}
	if valStart == -1 {
		return ""
	}

	// Find the closing quote, handling escaped quotes properly
	rest := sub[valStart:]
	valEnd := findUnescapedQuote(rest)
	if valEnd == -1 {
		return ""
	}

	return rest[:valEnd]
}

// findUnescapedQuote finds the position of the first unescaped `"` in s.
func findUnescapedQuote(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // skip escaped character
			continue
		}
		if s[i] == '"' {
			return i
		}
	}
	return -1
}

// detectSubmissions finds submitted messages by looking for text length drops.
// When the editor text shrinks significantly, the previous peak text was submitted.
func detectSubmissions(snapshots []tipTapSnapshot, afterTS float64) []tipTapSnapshot {
	var submitted []tipTapSnapshot
	var peak tipTapSnapshot // tracks the longest text in the current "typing run"

	for _, snap := range snapshots {
		// Update peak if text is growing or same length (typing in progress)
		if len(snap.Text) >= len(peak.Text) {
			peak = snap
			continue
		}

		// Text got shorter. Is it a significant drop (submission)?
		// Drop >50% OR new text is very short while peak was substantial
		if len(peak.Text) > minSubmissionLen && (len(snap.Text) < len(peak.Text)/2 || (len(peak.Text) > shortTextThreshold && len(snap.Text) < shortTextThreshold)) {
			// Only emit if the shrink happens after afterTS
			if snap.UpdatedAt > afterTS {
				peak.SubmittedAt = snap.UpdatedAt
				submitted = append(submitted, peak)
			}
			peak = snap // reset for next message
			continue
		}

		// Minor shrink (backspace/edit) — keep current peak
	}

	return submitted
}

// readChatModel reads the currently selected model from Desktop's Local Storage.
// The value is stored under key "sticky-model-selector" as e.g. "opus-4-6".
func readChatModel(desktopDir string) string {
	lsDir := filepath.Join(desktopDir, "Local Storage", "leveldb")
	entries, err := os.ReadDir(lsDir)
	if err != nil {
		return ""
	}

	needle := []byte("icky-model-selector") // matches "sticky-model-selector"
	modelPrefixes := []string{"opus-", "sonnet-", "haiku-"}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".ldb") && !strings.HasSuffix(name, ".log") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Size() > maxLDBFileSize {
			continue
		}

		data, err := os.ReadFile(filepath.Join(lsDir, name))
		if err != nil {
			continue
		}

		idx := bytes.Index(data, needle)
		if idx == -1 {
			continue
		}

		// Search for model slug near this key
		searchEnd := idx + chatModelSearchWindow
		if searchEnd > len(data) {
			searchEnd = len(data)
		}
		chunk := data[idx:searchEnd]

		// Clean to printable chars
		clean := make([]byte, 0, len(chunk))
		for _, b := range chunk {
			if b >= 32 && b < 127 {
				clean = append(clean, b)
			}
		}
		s := string(clean)

		// Find model slug (e.g. opus-4-6, sonnet-4-6)
		for _, prefix := range modelPrefixes {
			pidx := strings.Index(s, prefix)
			if pidx == -1 {
				continue
			}
			// Extract the full slug
			slugEnd := pidx
			for slugEnd < len(s) && (s[slugEnd] == '-' || (s[slugEnd] >= '0' && s[slugEnd] <= '9') || (s[slugEnd] >= 'a' && s[slugEnd] <= 'z')) {
				slugEnd++
			}
			slug := s[pidx:slugEnd]
			if len(slug) > 5 {
				return "claude-" + slug
			}
		}
	}
	return ""
}

// parseTimestamp parses a millisecond-epoch timestamp string.
// Returns 0 for empty, non-numeric, or implausible values.
// Valid range: 2020-01-01 to 2040-01-01 (ms epoch).
func parseTimestamp(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var result float64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		result = result*10 + float64(c-'0')
	}
	// Plausibility check: must be a 13-digit ms epoch in 2020-2040 range
	const (
		minTS = 1577836800000 // 2020-01-01T00:00:00Z
		maxTS = 2208988800000 // 2040-01-01T00:00:00Z
	)
	if result < minTS || result > maxTS {
		return 0
	}
	return result
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
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[claude] Warning: cannot resolve home directory for Desktop path: %v", err)
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude")
	default:
		return filepath.Join(home, ".config", "Claude")
	}
}
