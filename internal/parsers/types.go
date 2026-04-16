package parsers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
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
	// Identity is set by parsers that know the account/org from their data source
	// (e.g., Desktop parser extracts it from the directory path). When set, main.go
	// uses this instead of the global identity lookup.
	Identity     *AccountIdentity `json:"-"`
}

// Parser collects AI conversation records from a local tool.
type Parser interface {
	Name() string
	SetLookback(hours int) // limit to sessions modified within N hours
	Collect(state map[string]any) ([]Record, map[string]any, error)
}

// AccountIdentity holds the AI account info extracted from local tool data.
type AccountIdentity struct {
	AccountUUID      string `json:"account_uuid,omitempty"`
	OrganizationUUID string `json:"organization_uuid,omitempty"`
	Tool             string `json:"tool"` // "claude-code", "cursor", etc.
}

// uuidRe validates lowercase UUID format for session directory names.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ReadClaudeIdentities returns the CURRENT account identities for
// Claude Code (from statsig cache) and Claude Desktop (from the most
// recently modified session directory).
//
// Each identity carries a Tool field matching the collector source it
// governs, so the server can determine governance per-tool independently.
func ReadClaudeIdentities() []AccountIdentity {
	home, err := platform.HomeDir()
	if err != nil {
		return nil
	}

	var ids []AccountIdentity
	if id := readStatsigIdentity(home); id != nil {
		ids = append(ids, *id)
	}
	if id := readDesktopIdentity(home); id != nil {
		ids = append(ids, *id)
	}
	return ids
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

// readDesktopIdentity extracts the CURRENT Claude Desktop account identity
// from the most recently modified session directory.
//
// Claude Desktop stores sessions at:
//   {appDataDir}/local-agent-mode-sessions/{account_uuid}/{org_uuid}/
//
// The most recently modified {account}/{org} pair is the active session.
// Only the single most-recent pair is returned to avoid historical artifacts
// from previously-used accounts/orgs.
func readDesktopIdentity(home string) *AccountIdentity {
	appDir := platform.ClaudeDesktopDir(home)

	// Check both legacy and current Desktop session directories
	sessionsDirs := []string{
		filepath.Join(appDir, "claude-code-sessions"),
		filepath.Join(appDir, "local-agent-mode-sessions"),
	}

	var bestAccount, bestOrg string
	var bestMtime int64

	for _, sessionsDir := range sessionsDirs {
		accounts, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}

		for _, acctEntry := range accounts {
			if !acctEntry.IsDir() {
				continue
			}
			acctName := acctEntry.Name()
			if acctName == "skills-plugin" || !uuidRe.MatchString(acctName) {
				continue
			}

			orgs, err := os.ReadDir(filepath.Join(sessionsDir, acctName))
			if err != nil {
				continue
			}
			for _, orgEntry := range orgs {
				if !orgEntry.IsDir() {
					continue
				}
				orgName := orgEntry.Name()
				if !uuidRe.MatchString(orgName) {
					continue
				}
				info, err := orgEntry.Info()
				if err != nil {
					continue
				}
				if mt := info.ModTime().UnixNano(); mt > bestMtime {
					bestMtime = mt
					bestAccount = acctName
					bestOrg = orgName
				}
			}
		}
	}

	if bestAccount == "" {
		return nil
	}

	return &AccountIdentity{
		AccountUUID:      bestAccount,
		OrganizationUUID: bestOrg,
		Tool:             "claude-desktop",
	}
}
