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

// ReadClaudeIdentities extracts all account/org UUID pairs from both:
// 1. Claude Code's statsig cache (~/.claude/statsig/)
// 2. Claude Desktop session paths (~/Library/Application Support/Claude/local-agent-mode-sessions/{account}/{org}/)
func ReadClaudeIdentities() []AccountIdentity {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var identities []AccountIdentity

	// Source 1: statsig cache
	if id := readStatsigIdentity(home); id != nil {
		key := id.AccountUUID + ":" + id.OrganizationUUID
		if !seen[key] {
			identities = append(identities, *id)
			seen[key] = true
		}
	}

	// Source 2: Claude Desktop session directory structure
	// Path: local-agent-mode-sessions/{accountUUID}/{orgUUID}/
	sessionBase := filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions")
	accounts, err := os.ReadDir(sessionBase)
	if err == nil {
		for _, acct := range accounts {
			if !acct.IsDir() || acct.Name() == "skills-plugin" {
				continue
			}
			acctPath := filepath.Join(sessionBase, acct.Name())
			orgs, err := os.ReadDir(acctPath)
			if err != nil {
				continue
			}
			for _, org := range orgs {
				if !org.IsDir() {
					continue
				}
				key := acct.Name() + ":" + org.Name()
				if seen[key] {
					continue
				}
				identities = append(identities, AccountIdentity{
					AccountUUID:      acct.Name(),
					OrganizationUUID: org.Name(),
					Tool:             "claude-desktop",
				})
				seen[key] = true
			}
		}
	}

	// Source 3: Linux path
	linuxBase := filepath.Join(home, ".config", "Claude", "local-agent-mode-sessions")
	if linuxAccounts, err := os.ReadDir(linuxBase); err == nil {
		for _, acct := range linuxAccounts {
			if !acct.IsDir() || acct.Name() == "skills-plugin" {
				continue
			}
			acctPath := filepath.Join(linuxBase, acct.Name())
			orgs, _ := os.ReadDir(acctPath)
			for _, org := range orgs {
				if !org.IsDir() {
					continue
				}
				key := acct.Name() + ":" + org.Name()
				if seen[key] {
					continue
				}
				identities = append(identities, AccountIdentity{
					AccountUUID:      acct.Name(),
					OrganizationUUID: org.Name(),
					Tool:             "claude-desktop",
				})
				seen[key] = true
			}
		}
	}

	return identities
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

	var evalFile string
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), "statsig.cached.evaluations") {
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
