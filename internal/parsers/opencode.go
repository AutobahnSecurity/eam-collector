package parsers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type OpenCodeParser struct {
	dbPath   string
	lookback time.Duration
}

func NewOpenCodeParser() *OpenCodeParser {
	home, _ := os.UserHomeDir()
	return &OpenCodeParser{
		dbPath:   filepath.Join(home, ".opencode", "opencode.db"),
		lookback: 24 * time.Hour,
	}
}

func (p *OpenCodeParser) Name() string   { return "opencode" }
func (p *OpenCodeParser) DataDir() string { return p.dbPath }

func (p *OpenCodeParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *OpenCodeParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.dbPath); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	// Only collect if the DB was modified within the lookback window
	info, err := os.Stat(p.dbPath)
	if err != nil {
		return nil, prevState, fmt.Errorf("stat opencode db: %w", err)
	}
	if time.Since(info.ModTime()) > p.lookback {
		return nil, prevState, nil
	}

	var lastTS float64
	if v, ok := prevState["last_processed_ts"]; ok {
		if f, ok := v.(float64); ok {
			lastTS = f
		}
	}

	// Open in read-only mode to avoid interfering with OpenCode
	db, err := sql.Open("sqlite", p.dbPath+"?mode=ro")
	if err != nil {
		return nil, prevState, fmt.Errorf("open opencode db: %w", err)
	}
	defer db.Close()

	// Verify expected schema exists
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('sessions', 'messages')`).Scan(&tableCount); err != nil || tableCount < 2 {
		return nil, prevState, fmt.Errorf("schema changed: sessions/messages tables not found")
	}

	// Query sessions updated within lookback window, joined with their messages.
	// OpenCode stores timestamps as Unix milliseconds.
	cutoffMS := time.Now().Add(-p.lookback).UnixMilli()
	rows, err := db.Query(`
		SELECT s.id, m.role, m.parts, m.model, m.created_at,
		       s.prompt_tokens, s.completion_tokens, s.cost
		FROM messages m
		JOIN sessions s ON m.session_id = s.id
		WHERE s.updated_at > ?
		  AND m.created_at > ?
		  AND m.role IN ('user', 'assistant')
		ORDER BY m.created_at ASC
	`, cutoffMS, int64(lastTS))
	if err != nil {
		return nil, prevState, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var records []Record
	var maxTS float64
	// Track per-session token totals (only emit once per session)
	sessionTokens := make(map[string]bool)

	for rows.Next() {
		var sessionID, role, model sql.NullString
		var partsJSON sql.NullString
		var createdAt int64
		var promptTokens, completionTokens sql.NullInt64
		var cost sql.NullFloat64

		if err := rows.Scan(&sessionID, &role, &partsJSON, &model, &createdAt,
			&promptTokens, &completionTokens, &cost); err != nil {
			continue
		}

		content := extractOpenCodeContent(partsJSON.String)
		if content == "" {
			continue
		}

		ts := time.UnixMilli(createdAt).UTC().Format(time.RFC3339)
		sid := fmt.Sprintf("collector:opencode:%s", sessionID.String)

		r := Record{
			Source:    "opencode",
			SessionID: sid,
			Timestamp: ts,
			Role:      role.String,
			Content:   content,
			Model:     model.String,
			AIVendor:  "OpenCode",
		}

		// Attach token counts to the first record per session
		if !sessionTokens[sessionID.String] {
			r.InputTokens = int(promptTokens.Int64)
			r.OutputTokens = int(completionTokens.Int64)
			r.Cost = cost.Float64
			sessionTokens[sessionID.String] = true
		}

		records = append(records, r)

		if float64(createdAt) > maxTS {
			maxTS = float64(createdAt)
		}
	}

	if maxTS > lastTS {
		newState["last_processed_ts"] = maxTS
	} else {
		newState["last_processed_ts"] = lastTS
	}

	return records, newState, rows.Err()
}

// extractOpenCodeContent parses the parts JSON array from OpenCode messages.
// Format: [{"type": "text", "text": "..."}, ...]
func extractOpenCodeContent(partsJSON string) string {
	if partsJSON == "" {
		return ""
	}

	// Try as string first (simple text)
	var s string
	if err := json.Unmarshal([]byte(partsJSON), &s); err == nil {
		return s
	}

	// Try as array of content parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(partsJSON), &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}

	return ""
}
