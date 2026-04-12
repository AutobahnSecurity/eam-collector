package parsers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeLine represents a single JSONL line from Claude Code session files.
type claudeLine struct {
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
}

type ClaudeParser struct {
	baseDir  string // defaults to ~/.claude/projects/
	lookback time.Duration
}

func NewClaudeParser() *ClaudeParser {
	home, _ := os.UserHomeDir()
	return &ClaudeParser{
		baseDir:  filepath.Join(home, ".claude", "projects"),
		lookback: 24 * time.Hour,
	}
}

func (p *ClaudeParser) Name() string    { return "claude_code" }
func (p *ClaudeParser) DataDir() string  { return p.baseDir }

func (p *ClaudeParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *ClaudeParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	// Restore file offsets from previous state
	offsets := make(map[string]float64)
	if raw, ok := prevState["file_offsets"]; ok {
		if m, ok := raw.(map[string]any); ok {
			for k, v := range m {
				if f, ok := v.(float64); ok {
					offsets[k] = f
				}
			}
		}
	}

	// Find JSONL files modified within lookback window
	files, err := findJSONLFiles(p.baseDir, p.lookback)
	if err != nil {
		return nil, prevState, fmt.Errorf("scan claude dir: %w", err)
	}

	var records []Record
	newOffsets := make(map[string]float64)

	for _, path := range files {
		prevOffset := int64(offsets[path])
		recs, newOffset, err := p.parseFile(path, prevOffset)
		if err != nil {
			log.Printf("[claude] Error parsing %s: %v", path, err)
			newOffsets[path] = float64(prevOffset)
			continue
		}
		records = append(records, recs...)
		newOffsets[path] = float64(newOffset)
	}

	newState["file_offsets"] = newOffsets
	return records, newState, nil
}

func (p *ClaudeParser) parseFile(path string, offset int64) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	// Get file size — skip if unchanged
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if info.Size() <= offset {
		return nil, offset, nil
	}

	// Seek to last known position
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, err
		}
	}

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var cl claudeLine
		if err := json.Unmarshal(line, &cl); err != nil {
			continue // skip malformed lines
		}

		// Only process user and assistant messages
		if cl.Type != "user" && cl.Type != "assistant" {
			continue
		}

		content := extractContent(cl.Message.Content, cl.Type)
		if content == "" {
			continue
		}

		ts := cl.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}

		sessionID := cl.SessionID
		if sessionID == "" {
			// Derive from filename
			sessionID = filepath.Base(strings.TrimSuffix(path, ".jsonl"))
		}

		records = append(records, Record{
			Source:       "claude-code",
			SessionID:    fmt.Sprintf("collector:claude:%s", sessionID),
			Timestamp:    ts,
			Role:         cl.Message.Role,
			Content:      content,
			Model:        cl.Message.Model,
			InputTokens:  cl.Message.Usage.InputTokens,
			OutputTokens: cl.Message.Usage.OutputTokens,
			AIVendor:     "Anthropic",
		})
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	return records, newOffset, scanner.Err()
}

// extractContent handles both string (user) and array (assistant) content formats.
func extractContent(raw json.RawMessage, msgType string) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as string first (user messages)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as array (assistant messages: [{type: "text", text: "..."}, ...])
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

func findJSONLFiles(baseDir string, lookback time.Duration) ([]string, error) {
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible dirs
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			// Only read files modified within the lookback window (active sessions)
			if time.Since(info.ModTime()) < lookback {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}
