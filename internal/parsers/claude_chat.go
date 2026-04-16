package parsers

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
)

const (
	// maxLDBFileSize caps LevelDB file reads to prevent excessive memory use.
	maxLDBFileSize = 10 * 1024 * 1024 // 10 MB

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

// chatModelPrefixes are the known Claude model family prefixes.
// Extend this list when Anthropic ships new model families.
var chatModelPrefixes = []string{"opus-", "sonnet-", "haiku-"}

type tipTapSnapshot struct {
	Text        string
	UpdatedAt   float64
	SubmittedAt float64 // set by detectSubmissions to the shrink timestamp
}

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
	home, err := platform.HomeDir()
	if err != nil {
		log.Printf("[claude] Warning: %v", err)
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

// cleanToPrintable strips non-printable bytes from binary data,
// keeping only ASCII 32-126 (space through tilde).
func cleanToPrintable(data []byte) string {
	clean := make([]byte, 0, len(data))
	for _, b := range data {
		if b >= 32 && b < 127 {
			clean = append(clean, b)
		}
	}
	return string(clean)
}

// extractTipTapSnapshots extracts tipTap editor snapshots from LevelDB binary data.
//
// NOTE: This performs raw binary scanning because the LevelDB files cannot be
// safely opened with a library while Claude Desktop holds the lock. The format
// is fragile and may break if the Desktop app changes its IndexedDB schema.
func extractTipTapSnapshots(data []byte) []tipTapSnapshot {
	needle := []byte("tipTapEditorS")
	var snapshots []tipTapSnapshot

	for i := 0; i < len(data)-len(needle); {
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

		s := cleanToPrintable(data[start:end])

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
// Falls back to manual string scanning when the data is not valid JSON.
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
//
// State machine:
//   TYPING: len(snap.Text) >= len(peak.Text) → update peak, stay in TYPING
//   TYPING→SUBMIT: peak > minSubmissionLen AND (drop >50% OR peak large + snap small) → emit peak, reset
//   TYPING→MINOR_EDIT: small shrink → stay in TYPING with current peak
func detectSubmissions(snapshots []tipTapSnapshot, afterTS float64) []tipTapSnapshot {
	var submitted []tipTapSnapshot
	var peak tipTapSnapshot // tracks the longest text in the current "typing run"

	for _, snap := range snapshots {
		// TYPING: text is growing or same length
		if len(snap.Text) >= len(peak.Text) {
			peak = snap
			continue
		}

		// Text got shorter — check if this is a submission (significant drop)
		isPeakSubstantial := len(peak.Text) > minSubmissionLen
		isMajorDrop := len(snap.Text) < len(peak.Text)/2
		isResetToShort := len(peak.Text) > shortTextThreshold && len(snap.Text) < shortTextThreshold

		if isPeakSubstantial && (isMajorDrop || isResetToShort) {
			// SUBMIT: only emit if the shrink happens after afterTS
			if snap.UpdatedAt > afterTS {
				peak.SubmittedAt = snap.UpdatedAt
				submitted = append(submitted, peak)
			}
			peak = snap // reset for next message
			continue
		}

		// MINOR_EDIT: small shrink (backspace/edit) — keep current peak
	}

	return submitted
}

// readChatModel reads the currently selected model from Desktop's Local Storage.
// The value is stored under key "sticky-model-selector" as e.g. "opus-4-6".
//
// NOTE: This reads raw LevelDB binary files because the Desktop app holds the
// lock. The "icky-model-selector" needle omits the leading "st" because it may
// be separated by binary delimiters in the serialized format.
func readChatModel(desktopDir string) string {
	lsDir := filepath.Join(desktopDir, "Local Storage", "leveldb")
	entries, err := os.ReadDir(lsDir)
	if err != nil {
		return ""
	}

	needle := []byte("icky-model-selector") // matches "sticky-model-selector"

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

		s := cleanToPrintable(data[idx:searchEnd])

		// Find model slug (e.g. opus-4-6, sonnet-4-6)
		for _, prefix := range chatModelPrefixes {
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
	// Strip trailing non-digit characters
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}

	n, err := strconv.ParseInt(s[:end], 10, 64)
	if err != nil {
		return 0
	}

	// Plausibility check: must be a 13-digit ms epoch in 2020-2040 range
	const (
		minTS = 1577836800000 // 2020-01-01T00:00:00Z
		maxTS = 2208988800000 // 2040-01-01T00:00:00Z
	)
	if n < minTS || n > maxTS {
		return 0
	}
	return float64(n)
}
