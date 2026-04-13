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

// ClaudeLine represents a single JSONL line from Claude Code / Desktop audit files.
// Both tools use the same format.
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
	// Desktop audit.jsonl uses _audit_timestamp instead of timestamp
	AuditTimestamp string `json:"_audit_timestamp"`
}

type ClaudeParser struct {
	baseDir           string // defaults to ~/.claude/projects/
	lookback          time.Duration
	desktopSessionIDs map[string]bool // cliSessionIds owned by Desktop (skip in CLI parser)
}

func NewClaudeParser() *ClaudeParser {
	home, _ := os.UserHomeDir()
	return &ClaudeParser{
		baseDir:  filepath.Join(home, ".claude", "projects"),
		lookback: 24 * time.Hour,
	}
}

// SetDesktopSessionIDs sets the cliSessionIds that belong to Desktop code sessions.
// The Claude Code parser will skip these files to avoid double-counting.
func (p *ClaudeParser) SetDesktopSessionIDs(ids map[string]bool) {
	p.desktopSessionIDs = ids
}

func (p *ClaudeParser) Name() string   { return "claude_code" }
func (p *ClaudeParser) DataDir() string { return p.baseDir }

func (p *ClaudeParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *ClaudeParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	offsets := restoreOffsets(prevState)

	files, err := findJSONLFiles(p.baseDir, p.lookback)
	if err != nil {
		return nil, prevState, fmt.Errorf("scan claude dir: %w", err)
	}

	var records []Record
	newOffsets := make(map[string]float64)

	for _, path := range files {
		sessionID := filepath.Base(strings.TrimSuffix(path, ".jsonl"))

		// Skip JSONL files that belong to Desktop code sessions
		if p.desktopSessionIDs[sessionID] {
			continue
		}

		prevOffset := int64(offsets[path])
		recs, newOffset, err := ParseClaudeJSONLFile(path, prevOffset, "claude-code", "collector:claude:"+sessionID, "Anthropic")
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

// ── Shared helpers (used by claude.go and claude_desktop.go) ─────────

// ParseClaudeJSONLFile reads a Claude-format JSONL file from the given byte offset,
// parses user/assistant messages, and returns Records plus the new file offset.
// Both Claude Code and Desktop audit.jsonl use the same JSONL format.
//
// On first encounter (offset == 0), the file is skipped — we seek to the end
// and save the offset so only NEW messages written after this point are collected.
// This prevents shipping entire session histories on first run.
func ParseClaudeJSONLFile(path string, offset int64, source, sessionIDPrefix, aiVendor string) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if info.Size() <= offset {
		return nil, offset, nil
	}

	// First encounter: skip to end, only collect new data from next run
	if offset == 0 {
		return nil, info.Size(), nil
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer

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

		// Prefer timestamp, fall back to _audit_timestamp (Desktop)
		ts := cl.Timestamp
		if ts == "" {
			ts = cl.AuditTimestamp
		}
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}

		sessionID := sessionIDPrefix
		if cl.SessionID != "" && !strings.HasPrefix(sessionIDPrefix, "collector:claude-desktop:") {
			// For Claude Code, use the sessionId from the JSONL line
			sessionID = "collector:claude:" + cl.SessionID
		}

		model := cl.Message.Model
		if model == "<synthetic>" {
			model = ""
		}

		records = append(records, Record{
			Source:       source,
			SessionID:    sessionID,
			Timestamp:    ts,
			Role:         cl.Message.Role,
			Content:      content,
			Model:        model,
			InputTokens:  cl.Message.Usage.InputTokens,
			OutputTokens: cl.Message.Usage.OutputTokens,
			AIVendor:     aiVendor,
		})
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	return records, newOffset, scanner.Err()
}

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

// restoreOffsets extracts the file_offsets map from previous parser state.
func restoreOffsets(prevState map[string]any) map[string]float64 {
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
	return offsets
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
