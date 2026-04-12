package parsers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ClaudeDesktopParser reads Claude Desktop chat conversations from the
// Chromium IndexedDB LevelDB log. Claude Desktop stores editor draft
// snapshots as JSON in the LevelDB write-ahead log — each snapshot
// captures the user's message as they type it. The final (longest)
// snapshot before a new shorter one begins is the sent message.

type ClaudeDesktopParser struct {
	idbDir   string
	lookback time.Duration
}

func NewClaudeDesktopParser() *ClaudeDesktopParser {
	return &ClaudeDesktopParser{
		idbDir:   claudeDesktopIDBPath(),
		lookback: 24 * time.Hour,
	}
}

func (p *ClaudeDesktopParser) Name() string    { return "claude_desktop" }
func (p *ClaudeDesktopParser) DataDir() string { return p.idbDir }

func (p *ClaudeDesktopParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

// tipTapSnapshot matches the editor draft JSON stored in IndexedDB.
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

func (p *ClaudeDesktopParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	logPath := filepath.Join(p.idbDir, "000003.log")

	info, err := os.Stat(logPath)
	if os.IsNotExist(err) {
		return nil, prevState, nil
	}
	if err != nil {
		return nil, prevState, fmt.Errorf("stat idb log: %w", err)
	}

	// Skip if the log hasn't been modified within the lookback window
	if time.Since(info.ModTime()) > p.lookback {
		return nil, prevState, nil
	}

	// Get previous offset and timestamp
	var prevOffset float64
	if v, ok := prevState["log_offset"]; ok {
		if f, ok := v.(float64); ok {
			prevOffset = f
		}
	}
	var lastTS float64
	if v, ok := prevState["last_ts"]; ok {
		if f, ok := v.(float64); ok {
			lastTS = f
		}
	}

	// Read the log file from previous offset
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, prevState, fmt.Errorf("read idb log: %w", err)
	}

	startOffset := int(prevOffset)
	if startOffset > len(data) {
		startOffset = 0 // log was rotated
	}

	text := string(data[startOffset:])

	// Extract all tipTapEditorState JSON snapshots
	snapshots := extractSnapshots(text)
	if len(snapshots) == 0 {
		newState["log_offset"] = float64(len(data))
		newState["last_ts"] = lastTS
		return nil, newState, nil
	}

	// Group snapshots into sent messages:
	// As the user types, snapshots grow in length. When the text gets significantly
	// shorter, the user submitted the previous message and started a new one.
	messages := extractSentMessages(snapshots, lastTS)

	// Try to detect the active model from the IndexedDB blob files
	model := detectDesktopModel(filepath.Dir(logPath))

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
		sessionID := fmt.Sprintf("collector:claude-desktop:chat-%d", int64(msg.Timestamp)/3600000) // hourly buckets

		records = append(records, Record{
			Source:    "claude-desktop",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      "user",
			Content:   msg.Text,
			Model:     model,
			AIVendor:  "Anthropic",
		})

		if msg.Timestamp > maxTS {
			maxTS = msg.Timestamp
		}
	}

	if maxTS > lastTS {
		newState["last_ts"] = maxTS
	} else {
		newState["last_ts"] = lastTS
	}
	newState["log_offset"] = float64(len(data))

	return records, newState, nil
}

type sentMessage struct {
	Text      string
	Timestamp float64
}

func extractSnapshots(text string) []tipTapSnapshot {
	// Find JSON objects matching the tipTap editor state pattern
	var snapshots []tipTapSnapshot

	// Simple approach: find all JSON objects with tipTapEditorState
	idx := 0
	for {
		start := strings.Index(text[idx:], `{"state":{"tipTapEditorState":`)
		if start == -1 {
			break
		}
		start += idx

		// Find the matching closing brace by tracking depth
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

	// Sort by timestamp
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

		// Detect message boundary: text got significantly shorter (new message started),
		// or there's a >10 second gap (user submitted and waited for response)
		if prevText != "" && (len(text) < len(prevText)/2 || (ts-prevTS > 10000 && len(text) < len(prevText))) {
			if len(prevText) > 3 && prevTS > afterTS {
				messages = append(messages, sentMessage{Text: prevText, Timestamp: prevTS})
			}
		}

		prevText = text
		prevTS = ts
	}

	// Last message being typed (may not be sent yet, but include it)
	if prevText != "" && len(prevText) > 3 && prevTS > afterTS {
		messages = append(messages, sentMessage{Text: prevText, Timestamp: prevTS})
	}

	return messages
}

// detectDesktopModel scans IndexedDB blob files for Claude model identifiers.
// The blob contains Blink-serialized conversation metadata including model names.
var modelRe = regexp.MustCompile(`claude-(?:sonnet|opus|haiku)-[0-9]+(?:-[0-9]+)?(?:-[0-9]{8})?`)

func detectDesktopModel(idbDir string) string {
	blobDir := filepath.Join(filepath.Dir(idbDir), "https_claude.ai_0.indexeddb.blob")

	// Find the most recently modified blob file
	var newestPath string
	var newestTime time.Time
	filepath.Walk(blobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newestPath = path
		}
		return nil
	})

	if newestPath == "" {
		return ""
	}

	data, err := os.ReadFile(newestPath)
	if err != nil {
		return ""
	}

	// Find all model names and return the most common one (likely the active model)
	matches := modelRe.FindAll(data, -1)
	if len(matches) == 0 {
		return ""
	}

	counts := make(map[string]int)
	for _, m := range matches {
		counts[string(m)]++
	}

	var best string
	var bestCount int
	for model, count := range counts {
		if count > bestCount {
			best = model
			bestCount = count
		}
	}
	return best
}

func claudeDesktopIDBPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude",
			"IndexedDB", "https_claude.ai_0.indexeddb.leveldb")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude",
			"IndexedDB", "https_claude.ai_0.indexeddb.leveldb")
	default:
		return filepath.Join(home, ".config", "Claude",
			"IndexedDB", "https_claude.ai_0.indexeddb.leveldb")
	}
}
