package parsers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// scannerBufSize is the buffer size for JSONL line scanning.
	scannerBufSize = 1024 * 1024 // 1 MB

	// maxJSONLReadSize caps how much new JSONL data is read per cycle (50 MB).
	// Prevents unbounded memory use if a file grew massively between cycles.
	maxJSONLReadSize = 50 * 1024 * 1024
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

		// Skip oversized lines (>1MB) to avoid json scanner overflow
		if len(line) > scannerBufSize {
			log.Printf("[claude] Skipping oversized line (%d bytes) in %s", len(line), filepath.Base(path))
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

// ExtractContent handles both string (user) and array (assistant) content formats.
func ExtractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.ReplaceAll(s, "\x00", "")
	}

	// Array of content blocks:
	// Assistant: [{type: "text", text: "..."}, {type: "tool_use", ...}]
	// User tool_result: [{type: "tool_result", content: "...", tool_use_id: "..."}]
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			} else if b.Type == "tool_result" && len(b.Content) > 0 {
				var resultStr string
				if json.Unmarshal(b.Content, &resultStr) == nil && resultStr != "" {
					parts = append(parts, resultStr)
				} else {
					var resultBlocks []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(b.Content, &resultBlocks) == nil {
						for _, rb := range resultBlocks {
							if rb.Type == "text" && rb.Text != "" {
								parts = append(parts, rb.Text)
							}
						}
					}
				}
			}
		}
		result := strings.Join(parts, "\n")
		return strings.ReplaceAll(result, "\x00", "")
	}

	return ""
}
