package parsers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type continueSession struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	History   []struct {
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"` // string or array
		} `json:"message"`
	} `json:"history"`
}

type ContinueParser struct {
	baseDir  string
	lookback time.Duration
}

func (p *ContinueParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func NewContinueParser() *ContinueParser {
	home, _ := os.UserHomeDir()
	return &ContinueParser{
		baseDir: filepath.Join(home, ".continue", "sessions"),
	}
}

func (p *ContinueParser) Name() string { return "continuedev" }

func (p *ContinueParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.baseDir); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	// Get already-processed session IDs
	processed := make(map[string]bool)
	if raw, ok := prevState["processed"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					processed[s] = true
				}
			}
		}
	}

	// Find session files
	files, err := filepath.Glob(filepath.Join(p.baseDir, "*.json"))
	if err != nil {
		return nil, prevState, err
	}

	var records []Record
	var newProcessed []string
	for k := range processed {
		newProcessed = append(newProcessed, k)
	}

	for _, path := range files {
		if filepath.Base(path) == "sessions.json" {
			continue // index file, skip
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var session continueSession
		if err := json.Unmarshal(data, &session); err != nil {
			continue
		}

		if session.SessionID == "" {
			continue
		}
		if processed[session.SessionID] {
			continue
		}

		for _, h := range session.History {
			role := h.Message.Role
			if role != "user" && role != "assistant" {
				continue
			}

			content := extractContinueContent(h.Message.Content)
			if content == "" {
				continue
			}

			records = append(records, Record{
				Source:    "continuedev",
				SessionID: fmt.Sprintf("collector:continuedev:%s", session.SessionID),
				Timestamp: "", // Continue.dev doesn't store per-message timestamps
				Role:      role,
				Content:   content,
				AIVendor:  "Continue",
			})
		}

		newProcessed = append(newProcessed, session.SessionID)
	}

	// Bound the processed set to prevent unbounded growth
	if len(newProcessed) > 5000 {
		newProcessed = newProcessed[len(newProcessed)-5000:]
	}
	newState["processed"] = newProcessed
	return records, newState, nil
}

func extractContinueContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try array of content parts
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
		if len(texts) > 0 {
			return texts[0]
		}
	}

	return ""
}
