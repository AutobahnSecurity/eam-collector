package parsers

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
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
	home, err := platform.HomeDir()
	if err != nil {
		log.Printf("[copilot] Warning: %v", err)
		home = ""
	}
	return &CopilotParser{
		baseDir:  platform.CopilotBaseDir(home),
		lookback: 24 * time.Hour,
	}
}

func (p *CopilotParser) Name() string { return "copilot" }

func (p *CopilotParser) Collect(prevState map[string]any) ([]Record, map[string]any, error) {
	newState := make(map[string]any)

	if _, err := os.Stat(p.baseDir); os.IsNotExist(err) {
		return nil, prevState, nil
	}

	// Get previously processed file mtimes and request counts
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
	reqCounts := make(map[string]float64)
	if raw, ok := prevState["request_counts"]; ok {
		if m, ok := raw.(map[string]any); ok {
			for k, v := range m {
				if f, ok := v.(float64); ok {
					reqCounts[k] = f
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
	newReqCounts := make(map[string]float64)

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
			newReqCounts[path] = reqCounts[path]
			continue // unchanged
		}

		recs, err := p.parseFile(path)
		if err != nil {
			newMtimes[path] = mtimes[path]
			newReqCounts[path] = reqCounts[path]
			continue
		}

		// Only emit records beyond what we've already sent for this file.
		// This avoids re-sending the entire session when a new message arrives.
		prevCount := int(reqCounts[path])
		if prevCount < len(recs) {
			records = append(records, recs[prevCount:]...)
		}
		newMtimes[path] = mtime
		newReqCounts[path] = float64(len(recs))
	}

	newState["file_mtimes"] = newMtimes
	newState["request_counts"] = newReqCounts
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

