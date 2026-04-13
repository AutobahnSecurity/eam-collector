package parsers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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

func (p *CursorParser) Name() string    { return "cursor" }
func (p *CursorParser) DataDir() string  { return p.dbPath }

func (p *CursorParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *CursorParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.dbPath); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	// Only collect if Cursor's database was modified within the lookback window.
	// This prevents reporting stale data from an installed-but-unused Cursor.
	info, err := os.Stat(p.dbPath)
	if err != nil {
		return nil, prevState, fmt.Errorf("stat cursor db: %w", err)
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

	// First encounter: skip to current time, only collect new data from next run
	if lastTS == 0 {
		newState["last_processed_ts"] = float64(time.Now().UnixMilli())
		newState["processed_bubbles"] = []string{}
		return nil, newState, nil
	}

	db, err := sql.Open("sqlite", p.dbPath+"?mode=ro")
	if err != nil {
		return nil, prevState, fmt.Errorf("open cursor db: %w", err)
	}
	defer db.Close()

	var records []Record
	var maxTS float64

	// Strategy: read composerData entries. Depending on version:
	// - Old (v1-v3): messages in separate bubbleId: rows
	// - New (v14+): messages inline in the conversation field or text/richText

	// Schema check: verify cursorDiskKV table exists and has expected data
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cursorDiskKV'`).Scan(&tableCount); err != nil || tableCount == 0 {
		// Table doesn't exist — Cursor may have changed its schema
		return nil, prevState, fmt.Errorf("schema changed: cursorDiskKV table not found (Cursor may have updated)")
	}

	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, prevState, fmt.Errorf("query composers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(value, &raw); err != nil {
			continue
		}

		// Extract composerId and createdAt
		composerID := strings.TrimPrefix(key, "composerData:")
		var createdAt float64
		if v, ok := raw["createdAt"]; ok {
			json.Unmarshal(v, &createdAt)
		}
		if createdAt <= lastTS {
			continue
		}
		// Only process sessions within lookback window (active sessions)
		if time.Since(time.UnixMilli(int64(createdAt))) > p.lookback {
			continue
		}

		ts := time.UnixMilli(int64(createdAt)).UTC().Format(time.RFC3339)
		sessionID := fmt.Sprintf("collector:cursor:%s", composerID)

		// Try to extract from inline conversation (v14+)
		if convRaw, ok := raw["conversation"]; ok {
			recs := p.parseConversation(convRaw, sessionID, ts)
			records = append(records, recs...)
		}

		// Try to extract from inline text (v1 with embedded conversation)
		if textRaw, ok := raw["text"]; ok {
			var text string
			if json.Unmarshal(textRaw, &text) == nil && text != "" {
				records = append(records, Record{
					Source:    "cursor",
					SessionID: sessionID,
					Timestamp: ts,
					Role:      "user",
					Content:   text,
					AIVendor:  "Cursor",
				})
			}
		}

		if createdAt > maxTS {
			maxTS = createdAt
		}
	}

	// Also check for bubbles (older format) — track processed keys to avoid re-sending.
	// Skip bubble reading entirely if processedBubbles is empty (first run after skip) —
	// prevents dumping all historical bubble data.
	processedBubbles := make(map[string]bool)
	if raw, ok := prevState["processed_bubbles"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					processedBubbles[s] = true
				}
			}
		}
	}
	if len(processedBubbles) > 0 {
		bubbleRecords, newBubbleKeys, err := p.readBubbles(db, processedBubbles)
		if err != nil {
			log.Printf("[cursor] Error reading bubbles: %v", err)
		} else {
			records = append(records, bubbleRecords...)
		}
		// Keep only last 10000 bubble keys to bound state size
		allKeys := make([]string, 0, len(processedBubbles)+len(newBubbleKeys))
		for k := range processedBubbles {
			allKeys = append(allKeys, k)
		}
		allKeys = append(allKeys, newBubbleKeys...)
		if len(allKeys) > 10000 {
			allKeys = allKeys[len(allKeys)-10000:]
		}
		newState["processed_bubbles"] = allKeys
	} else {
		// First run after skip — mark all current bubbles as processed
		// so next cycle only picks up new ones
		allKeys, _ := p.getAllBubbleKeys(db)
		newState["processed_bubbles"] = allKeys
	}

	if maxTS > lastTS {
		newState["last_processed_ts"] = maxTS
	} else {
		newState["last_processed_ts"] = lastTS
	}

	return records, newState, nil
}

// parseConversation extracts messages from the inline conversation array (v14+)
func (p *CursorParser) parseConversation(raw json.RawMessage, sessionID, ts string) []Record {
	var conversation []struct {
		Type    int    `json:"type"` // 1=user, 2=assistant
		Text    string `json:"text"`
		BubbleID string `json:"bubbleId"`
	}
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return nil
	}

	var records []Record
	for _, msg := range conversation {
		if msg.Text == "" {
			continue
		}
		role := "user"
		if msg.Type == 2 {
			role = "assistant"
		}
		records = append(records, Record{
			Source:    "cursor",
			SessionID: sessionID,
			Timestamp: ts,
			Role:      role,
			Content:   msg.Text,
			AIVendor:  "Cursor",
		})
	}
	return records
}

// readBubbles reads from the old bubbleId: format (v1-v3 composers).
// Returns records and list of newly processed keys.
func (p *CursorParser) readBubbles(db *sql.DB, processed map[string]bool) ([]Record, []string, error) {
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'bubbleId:%'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var records []Record
	var newKeys []string
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}

		if processed[key] {
			continue
		}

		parts := strings.SplitN(key, ":", 3)
		if len(parts) < 3 {
			continue
		}
		composerID := parts[1]

		var bubble struct {
			Type int    `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(value, &bubble); err != nil {
			continue
		}
		if bubble.Text == "" {
			continue
		}

		role := "user"
		if bubble.Type == 2 {
			role = "assistant"
		}

		records = append(records, Record{
			Source:    "cursor",
			SessionID: fmt.Sprintf("collector:cursor:%s", composerID),
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Role:      role,
			Content:   bubble.Text,
			AIVendor:  "Cursor",
		})
		newKeys = append(newKeys, key)
	}

	return records, newKeys, rows.Err()
}

// getAllBubbleKeys returns all current bubbleId keys without reading their content.
// Used on first run to mark all existing bubbles as processed.
func (p *CursorParser) getAllBubbleKeys(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT key FROM cursorDiskKV WHERE key LIKE 'bubbleId:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) > 10000 {
		keys = keys[len(keys)-10000:]
	}
	return keys, rows.Err()
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
