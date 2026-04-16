package parsers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
)

type CursorParser struct {
	dbPath   string
	lookback time.Duration
}

func NewCursorParser() *CursorParser {
	home, err := platform.HomeDir()
	if err != nil {
		log.Printf("[cursor] Warning: %v", err)
		home = ""
	}
	return &CursorParser{
		dbPath:   platform.CursorDBPath(home),
		lookback: 24 * time.Hour,
	}
}

func (p *CursorParser) Name() string { return "cursor" }

func (p *CursorParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func (p *CursorParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.dbPath); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	var lastTS float64
	if v, ok := prevState["last_processed_ts"]; ok {
		if f, ok := v.(float64); ok {
			lastTS = f
		}
	}

	db, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		return nil, prevState, fmt.Errorf("open cursor db: %w", err)
	}
	defer db.Close()

	var records []Record
	var maxTS float64

	// Strategy: read composerData entries. Depending on version:
	// - Old (v1-v3): messages in separate bubbleId: rows
	// - New (v14+): messages inline in the conversation field or text/richText

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
			if err := json.Unmarshal(v, &createdAt); err != nil {
				log.Printf("[cursor] Cannot parse createdAt for %s: %v", composerID, err)
			}
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

		// Try to extract from inline conversation (v14+).
		// Only fall back to text field if conversation is absent or empty,
		// to avoid double-emitting when both fields coexist during upgrades.
		var extracted bool
		if convRaw, ok := raw["conversation"]; ok {
			recs := p.parseConversation(convRaw, sessionID, ts)
			if len(recs) > 0 {
				records = append(records, recs...)
				extracted = true
			}
		}

		if !extracted {
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
		}

		if createdAt > maxTS {
			maxTS = createdAt
		}
	}

	// Also check for bubbles (older format) — track processed keys to avoid re-sending
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

		// Use DB file mtime as best-effort timestamp for old-format bubbles
		// that lack per-message timestamps.
		var bubbleTS string
		if info, err := os.Stat(p.dbPath); err == nil {
			bubbleTS = info.ModTime().UTC().Format(time.RFC3339)
		} else {
			bubbleTS = time.Now().UTC().Format(time.RFC3339)
		}

		records = append(records, Record{
			Source:    "cursor",
			SessionID: fmt.Sprintf("collector:cursor:%s", composerID),
			Timestamp: bubbleTS,
			Role:      role,
			Content:   bubble.Text,
			AIVendor:  "Cursor",
		})
		newKeys = append(newKeys, key)
	}

	return records, newKeys, rows.Err()
}

