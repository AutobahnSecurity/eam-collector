package parsers

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type CursorParser struct {
	dbPath   string
	lookback time.Duration
}

func NewCursorParser() *CursorParser {
	return &CursorParser{
		dbPath:   cursorDBPath(),
		lookback: 24 * time.Hour,
	}
}

func (p *CursorParser) Name() string   { return "cursor" }
func (p *CursorParser) DataDir() string { return p.dbPath }

func (p *CursorParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

// Cursor stores data in cursorDiskKV (SQLite):
//
//   composerData:{composerId} → JSON with createdAt, lastUpdatedAt, status
//   bubbleId:{composerId}:{bubbleId} → JSON with type (1=user, 2=assistant), text
//
// Strategy:
//   1. Find composers active within the lookback window (by lastUpdatedAt/createdAt)
//   2. Only fetch bubbles for those active composers
//   3. Track seen bubble keys per-composer to emit only new ones

func (p *CursorParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	if _, err := os.Stat(p.dbPath); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	info, err := os.Stat(p.dbPath)
	if err != nil {
		return nil, prevState, fmt.Errorf("stat cursor db: %w", err)
	}
	if time.Since(info.ModTime()) > p.lookback {
		return nil, prevState, nil
	}

	db, err := sql.Open("sqlite", p.dbPath+"?mode=ro")
	if err != nil {
		return nil, prevState, fmt.Errorf("open cursor db: %w", err)
	}
	defer db.Close()

	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cursorDiskKV'`).Scan(&tableCount); err != nil || tableCount == 0 {
		return nil, prevState, fmt.Errorf("schema changed: cursorDiskKV table not found")
	}

	// Step 1: Find active composers within the lookback window.
	// Uses millisecond epoch timestamps stored in composerData.
	cutoffMs := time.Now().Add(-p.lookback).UnixMilli()
	activeRows, err := db.Query(`
		SELECT json_extract(value, '$.composerId') as cid
		FROM cursorDiskKV
		WHERE key LIKE 'composerData:%'
		  AND COALESCE(
		    json_extract(value, '$.lastUpdatedAt'),
		    json_extract(value, '$.createdAt')
		  ) > ?
	`, cutoffMs)
	if err != nil {
		return nil, prevState, fmt.Errorf("query active composers: %w", err)
	}

	var activeComposers []string
	for activeRows.Next() {
		var cid string
		if err := activeRows.Scan(&cid); err == nil && cid != "" {
			activeComposers = append(activeComposers, cid)
		}
	}
	activeRows.Close()

	if len(activeComposers) == 0 {
		return nil, prevState, nil
	}

	// Restore seen bubble keys from previous state.
	// Handle both []any (from JSON round-trip) and []string (in-memory).
	seen := make(map[string]bool)
	if raw, ok := prevState["seen_keys"]; ok {
		switch arr := raw.(type) {
		case []any:
			for _, v := range arr {
				if s, ok := v.(string); ok {
					seen[s] = true
				}
			}
		case []string:
			for _, s := range arr {
				seen[s] = true
			}
		}
	}
	isFirstRun := prevState == nil || prevState["seen_keys"] == nil

	// Step 2: Fetch user bubbles only for active composers
	var records []Record
	var newKeys []string

	for _, composerID := range activeComposers {
		prefix := "bubbleId:" + composerID + ":"
		rows, err := db.Query(`
			SELECT key, value FROM cursorDiskKV
			WHERE key LIKE ? || '%'
			  AND json_extract(value, '$.type') = 1
			  AND json_extract(value, '$.text') != ''
		`, prefix)
		if err != nil {
			continue
		}

		for rows.Next() {
			var key string
			var value []byte
			if err := rows.Scan(&key, &value); err != nil || value == nil {
				continue
			}

			newKeys = append(newKeys, key)

			if seen[key] || isFirstRun {
				continue
			}

			text := extractJSONField(value, "text")
			if text == "" {
				continue
			}

			records = append(records, Record{
				Source:    "cursor",
				SessionID: fmt.Sprintf("collector:cursor:%s", composerID),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Role:      "user",
				Content:   text,
				AIVendor:  "Cursor",
			})
		}
		rows.Close()
	}

	// State: only track keys from active composers (old ones drop off naturally)
	newState := make(map[string]any)
	newState["seen_keys"] = newKeys
	return records, newState, nil
}

// extractJSONField extracts a string value for a key from a JSON object.
// Simple parser that avoids importing encoding/json.
func extractJSONField(data []byte, field string) string {
	needle := `"` + field + `":"`
	s := string(data)
	idx := strings.Index(s, needle)
	if idx == -1 {
		return ""
	}
	start := idx + len(needle)
	end := strings.Index(s[start:], `"`)
	if end == -1 {
		return ""
	}
	return s[start : start+end]
}

func cursorDBPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
