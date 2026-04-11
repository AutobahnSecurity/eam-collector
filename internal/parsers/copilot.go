package parsers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type copilotSession struct {
	SessionID  string `json:"sessionId"`
	Requests   []struct {
		RequestID string `json:"requestId"`
		Message   struct {
			Text string `json:"text"`
		} `json:"message"`
		Response []struct {
			Value string `json:"value"`
		} `json:"response"`
	} `json:"requests"`
	CreationDate    int64 `json:"creationDate"`
	LastMessageDate int64 `json:"lastMessageDate"`
}

type CopilotParser struct {
	baseDir  string
	lookback time.Duration
}

func (p *CopilotParser) SetLookback(hours int) {
	p.lookback = time.Duration(hours) * time.Hour
}

func NewCopilotParser() *CopilotParser {
	return &CopilotParser{
		baseDir: copilotBaseDir(),
	}
}

func (p *CopilotParser) Name() string { return "copilot" }

func (p *CopilotParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.baseDir); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	// Get previously processed file mtimes
	mtimes := make(map[string]string)
	if raw, ok := prevState["file_mtimes"]; ok {
		if m, ok := raw.(map[string]any); ok {
			for k, v := range m {
				if s, ok := v.(string); ok {
					mtimes[k] = s
				}
			}
		}
	}

	// Find chat session JSON files
	files, err := findCopilotFiles(p.baseDir)
	if err != nil {
		return nil, prevState, err
	}

	var records []Record
	newMtimes := make(map[string]string)

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		// Skip files outside lookback window
		if p.lookback > 0 && time.Since(info.ModTime()) > p.lookback {
			continue
		}

		mtime := info.ModTime().UTC().Format(time.RFC3339)
		if mtimes[path] == mtime {
			newMtimes[path] = mtime
			continue // unchanged
		}

		recs, err := p.parseFile(path)
		if err != nil {
			newMtimes[path] = mtimes[path] // keep old mtime on error
			continue
		}
		records = append(records, recs...)
		newMtimes[path] = mtime
	}

	newState["file_mtimes"] = newMtimes
	return records, newState, nil
}

func (p *CopilotParser) parseFile(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var session copilotSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	var records []Record
	for _, req := range session.Requests {
		if req.Message.Text != "" {
			ts := time.UnixMilli(session.CreationDate).UTC().Format(time.RFC3339)
			records = append(records, Record{
				Source:    "copilot",
				SessionID: fmt.Sprintf("collector:copilot:%s", session.SessionID),
				Timestamp: ts,
				Role:      "user",
				Content:   req.Message.Text,
				AIVendor:  "GitHub",
			})
		}

		for _, resp := range req.Response {
			if resp.Value != "" {
				ts := time.UnixMilli(session.LastMessageDate).UTC().Format(time.RFC3339)
				records = append(records, Record{
					Source:    "copilot",
					SessionID: fmt.Sprintf("collector:copilot:%s", session.SessionID),
					Timestamp: ts,
					Role:      "assistant",
					Content:   resp.Value,
					AIVendor:  "GitHub",
				})
			}
		}
	}

	return records, nil
}

func findCopilotFiles(baseDir string) ([]string, error) {
	pattern := filepath.Join(baseDir, "*", "chatSessions", "*.json")
	return filepath.Glob(pattern)
}

func copilotBaseDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "workspaceStorage")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "workspaceStorage")
	default:
		return filepath.Join(home, ".config", "Code", "User", "workspaceStorage")
	}
}
