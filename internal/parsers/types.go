package parsers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Record is the unified output format for all parsers.
// Maps to the EAM ingest API payload schema.
type Record struct {
	Source       string  `json:"source"`
	SessionID    string  `json:"session_id"`
	Timestamp    string  `json:"timestamp"`
	Role         string  `json:"role"` // "user" or "assistant"
	Content      string  `json:"content"`
	Model        string  `json:"model,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
	AIVendor     string  `json:"ai_vendor"`
}

// Health reports the status of a parser after collection.
type Health struct {
	Parser   string `json:"parser"`
	Status   string `json:"status"`    // "ok", "degraded", "error", "not_installed"
	Records  int    `json:"records"`
	Error    string `json:"error,omitempty"`
	DataPath string `json:"data_path,omitempty"`
}

// Parser collects AI conversation records from a local tool.
type Parser interface {
	Name() string
	SetLookback(hours int) // limit to sessions modified within N hours
	Collect(state map[string]any) ([]Record, map[string]any, error)
	DataDir() string // returns the path this parser reads from
}

// AccountIdentity holds the AI account info extracted from local tool data.
type AccountIdentity struct {
	AccountUUID      string `json:"account_uuid,omitempty"`
	OrganizationUUID string `json:"organization_uuid,omitempty"`
	Tool             string `json:"tool"` // "claude-code", "cursor", etc.
}

// ReadClaudeIdentities returns the CURRENT Claude account identity from
// the statsig cache. This reflects the actively-logged-in account only.
//
// Previously this also scanned Desktop session directories
// (local-agent-mode-sessions/{account}/{org}/), but those contain
// every org UUID ever used on this machine — historical artifacts that
// caused personal sessions to be incorrectly marked as governed.
func ReadClaudeIdentities() []AccountIdentity {
	home, _ := os.UserHomeDir()

	if id := readStatsigIdentity(home); id != nil {
		return []AccountIdentity{*id}
	}

	return nil
}

// ReadClaudeIdentity returns the first identity found (backward compat).
func ReadClaudeIdentity() *AccountIdentity {
	ids := ReadClaudeIdentities()
	if len(ids) == 0 {
		return nil
	}
	return &ids[0]
}

func readStatsigIdentity(home string) *AccountIdentity {
	statsigDir := filepath.Join(home, ".claude", "statsig")

	files, err := os.ReadDir(statsigDir)
	if err != nil {
		return nil
	}

	// Pick the most recently modified evaluation cache file.
	// Multiple files may exist for different accounts.
	var evalFile string
	var newestMtime int64
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), "statsig.cached.evaluations") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		if mt := info.ModTime().UnixNano(); mt > newestMtime {
			newestMtime = mt
			evalFile = filepath.Join(statsigDir, f.Name())
		}
	}
	if evalFile == "" {
		return nil
	}

	data, err := os.ReadFile(evalFile)
	if err != nil {
		return nil
	}

	var outer struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil
	}

	var inner struct {
		EvaluatedKeys struct {
			CustomIDs struct {
				AccountUUID      string `json:"accountUUID"`
				OrganizationUUID string `json:"organizationUUID"`
			} `json:"customIDs"`
		} `json:"evaluated_keys"`
	}
	if err := json.Unmarshal([]byte(outer.Data), &inner); err != nil {
		return nil
	}

	if inner.EvaluatedKeys.CustomIDs.AccountUUID == "" {
		return nil
	}

	return &AccountIdentity{
		AccountUUID:      inner.EvaluatedKeys.CustomIDs.AccountUUID,
		OrganizationUUID: inner.EvaluatedKeys.CustomIDs.OrganizationUUID,
		Tool:             "claude-code",
	}
}
