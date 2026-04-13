package parsers

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexParser reads OpenAI Codex CLI sessions using the SQLite metadata
// database (state_5.sqlite) as an index, then reads conversation JSONL
// files at the paths stored in the threads table.
type CodexParser struct {
	homeDir  string // ~/.codex or $CODEX_HOME
	lookback time.Duration
}

func NewCodexParser() *CodexParser {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".codex")
	if env := os.Getenv("CODEX_HOME"); env != "" {
		base = env
	}
	return &CodexParser{
		homeDir:  base,
		lookback: 24 * time.Hour,
	}
}

func (p *CodexParser) Name() string   { return "codex" }
func (p *CodexParser) DataDir() string { return filepath.Join(p.homeDir, "sessions") }

func (p *CodexParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *CodexParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	// Find the SQLite database — Codex uses state_N.sqlite with version suffix
	dbPath := p.findStateDB()
	if dbPath == "" {
		return nil, prevState, nil
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, prevState, fmt.Errorf("open codex db: %w", err)
	}
	defer db.Close()

	// Verify schema
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='threads'`).Scan(&tableCount); err != nil || tableCount == 0 {
		return nil, prevState, fmt.Errorf("schema changed: threads table not found")
	}

	// Query active threads within lookback
	cutoffMS := time.Now().Add(-p.lookback).UnixMilli()
	rows, err := db.Query(`
		SELECT id, rollout_path, COALESCE(model, ''), COALESCE(model_provider, ''), tokens_used
		FROM threads
		WHERE updated_at > ? AND archived = 0
		ORDER BY updated_at DESC
	`, cutoffMS)
	if err != nil {
		return nil, prevState, fmt.Errorf("query threads: %w", err)
	}
	defer rows.Close()

	offsets := restoreOffsets(prevState)
	var records []Record
	newOffsets := make(map[string]float64)

	for rows.Next() {
		var id, rolloutPath, model, modelProvider string
		var tokensUsed int64
		if err := rows.Scan(&id, &rolloutPath, &model, &modelProvider, &tokensUsed); err != nil {
			continue
		}

		if _, err := os.Stat(rolloutPath); os.IsNotExist(err) {
			continue
		}

		prevOffset := int64(offsets[rolloutPath])
		vendor := codexVendorName(modelProvider)

		recs, newOffset, err := p.parseCodexJSONL(rolloutPath, prevOffset, id, model, vendor)
		if err != nil {
			log.Printf("[codex] Error parsing %s: %v", rolloutPath, err)
			newOffsets[rolloutPath] = float64(prevOffset)
			continue
		}
		records = append(records, recs...)
		newOffsets[rolloutPath] = float64(newOffset)
	}

	newState["file_offsets"] = newOffsets
	return records, newState, rows.Err()
}

// findStateDB finds the Codex state SQLite database (state_N.sqlite).
func (p *CodexParser) findStateDB() string {
	entries, err := os.ReadDir(p.homeDir)
	if err != nil {
		return ""
	}
	// Find the highest-versioned state_N.sqlite
	var best string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "state_") && strings.HasSuffix(name, ".sqlite") {
			candidate := filepath.Join(p.homeDir, name)
			if best == "" || name > best {
				best = candidate
			}
		}
	}
	return best
}

// codexJSONLLine represents a single line from a Codex session JSONL file.
type codexJSONLLine struct {
	Timestamp string          `json:"timestamp"` // ISO 8601
	Type      string          `json:"type"`      // "session_meta", "response_item", "event_msg", "turn_context"
	Payload   json.RawMessage `json:"payload"`
}

// codexResponsePayload is the payload for type=response_item lines.
type codexResponsePayload struct {
	Role    string `json:"role"` // "user", "developer", "assistant"
	Content []struct {
		Type string `json:"type"` // "input_text", "output_text"
		Text string `json:"text"`
	} `json:"content"`
}

func (p *CodexParser) parseCodexJSONL(path string, offset int64, threadID, model, vendor string) ([]Record, int64, error) {
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

	sessionID := fmt.Sprintf("collector:codex:%s", threadID)

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var cl codexJSONLLine
		if err := json.Unmarshal(line, &cl); err != nil {
			continue
		}

		if cl.Type != "response_item" {
			continue
		}

		var payload codexResponsePayload
		if err := json.Unmarshal(cl.Payload, &payload); err != nil {
			continue
		}

		// Map roles: "user" → user, "assistant" → assistant, skip "developer" (system context)
		role := ""
		switch payload.Role {
		case "user":
			role = "user"
		case "assistant":
			role = "assistant"
		default:
			continue
		}

		content := extractCodexText(payload.Content)
		if content == "" {
			continue
		}

		ts := cl.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}

		records = append(records, Record{
			Source:    "codex",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      role,
			Content:   content,
			Model:     model,
			AIVendor:  vendor,
		})
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	return records, newOffset, scanner.Err()
}

func extractCodexText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var parts []string
	for _, c := range content {
		if (c.Type == "input_text" || c.Type == "output_text") && c.Text != "" {
			// Skip system context XML tags
			if strings.HasPrefix(c.Text, "<permissions") ||
				strings.HasPrefix(c.Text, "<app-context>") ||
				strings.HasPrefix(c.Text, "<environment_context>") ||
				strings.HasPrefix(c.Text, "<turn_aborted>") {
				continue
			}
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func codexVendorName(provider string) string {
	switch strings.ToLower(provider) {
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "google":
		return "Google"
	default:
		if provider != "" {
			return provider
		}
		return "OpenAI"
	}
}
