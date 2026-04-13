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

// codexSessionMeta is the first line of each Codex JSONL session file.
type codexSessionMeta struct {
	ID            string `json:"id"`
	ModelProvider string `json:"model_provider"`
	AgentRole     string `json:"agent_role"`
	CLIVersion    string `json:"cli_version"`
}

// codexLine represents a single JSONL line from a Codex session file.
type codexLine struct {
	Timestamp string          `json:"timestamp"` // ISO 8601
	Item      json.RawMessage `json:"item"`
}

// codexItem is the tagged union wrapper for Codex events.
type codexItem struct {
	Type string `json:"type"`
	// For response items
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// For event messages
	Event string `json:"event"`
}

type CodexParser struct {
	baseDir  string
	lookback time.Duration
}

func NewCodexParser() *CodexParser {
	home, _ := os.UserHomeDir()
	// CODEX_HOME overrides the default
	base := filepath.Join(home, ".codex")
	if env := os.Getenv("CODEX_HOME"); env != "" {
		base = env
	}
	return &CodexParser{
		baseDir:  filepath.Join(base, "sessions"),
		lookback: 24 * time.Hour,
	}
}

func (p *CodexParser) Name() string   { return "codex" }
func (p *CodexParser) DataDir() string { return p.baseDir }

func (p *CodexParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *CodexParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.baseDir); os.IsNotExist(err) {
		return nil, prevState, nil
	}

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
	files, err := findCodexFiles(p.baseDir, p.lookback)
	if err != nil {
		return nil, prevState, fmt.Errorf("scan codex dir: %w", err)
	}

	var records []Record
	newOffsets := make(map[string]float64)

	for _, path := range files {
		prevOffset := int64(offsets[path])
		recs, newOffset, err := p.parseFile(path, prevOffset)
		if err != nil {
			log.Printf("[codex] Error parsing %s: %v", path, err)
			newOffsets[path] = float64(prevOffset)
			continue
		}
		records = append(records, recs...)
		newOffsets[path] = float64(newOffset)
	}

	newState["file_offsets"] = newOffsets
	return records, newState, nil
}

func (p *CodexParser) parseFile(path string, offset int64) ([]Record, int64, error) {
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

	// For new files (offset 0), read the first line to get session metadata
	var meta codexSessionMeta
	var metaRead bool
	if offset == 0 {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 512*1024), 512*1024)
		if scanner.Scan() {
			var firstLine struct {
				Item json.RawMessage `json:"item"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &firstLine); err == nil {
				json.Unmarshal(firstLine.Item, &meta)
				metaRead = true
			}
		}
		// Reset to beginning so the main loop processes all lines
		f.Seek(0, io.SeekStart)
	}

	// Seek to last known position
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, err
		}
	}

	// Derive session ID from filename: rollout-<timestamp>-<uuid>.jsonl
	sessionID := fmt.Sprintf("collector:codex:%s", strings.TrimSuffix(filepath.Base(path), ".jsonl"))

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var cl codexLine
		if err := json.Unmarshal(line, &cl); err != nil {
			continue
		}

		// Parse the item to determine type
		var item codexItem
		if err := json.Unmarshal(cl.Item, &item); err != nil {
			// Item might be a SessionMeta (first line) — skip
			continue
		}

		// Extract content from response items
		role := ""
		switch {
		case item.Role == "user":
			role = "user"
		case item.Role == "assistant":
			role = "assistant"
		default:
			continue
		}

		content := extractCodexContent(item.Content)
		if content == "" {
			continue
		}

		ts := cl.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}

		model := ""
		if metaRead && meta.ModelProvider != "" {
			model = meta.ModelProvider
		}

		records = append(records, Record{
			Source:    "codex",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      role,
			Content:   content,
			Model:     model,
			AIVendor:  "OpenAI",
		})
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	return records, newOffset, scanner.Err()
}

// extractCodexContent handles Codex content which can be a string or array of content parts.
func extractCodexContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as array of content parts [{type: "text", text: "..."}, ...]
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}

	return ""
}

func findCodexFiles(baseDir string, lookback time.Duration) ([]string, error) {
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
