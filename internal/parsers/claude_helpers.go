package parsers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// desktopSessionMeta holds the fields we need from local_{uuid}.json.
type desktopSessionMeta struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	CWD            string `json:"cwd"`
	OriginCWD      string `json:"originCwd"`
	LastActivityAt int64  `json:"lastActivityAt"` // unix ms
	Model          string `json:"model"`
	IsArchived     bool   `json:"isArchived"`
}

// activeSession represents a Claude session that is currently active.
type activeSession struct {
	DataPath string           // path to JSONL file
	Source   string           // "claude-desktop" or "claude-code"
	Identity *AccountIdentity // from Desktop dir path or statsig
}

func restoreOffsets(prevState map[string]any) map[string]float64 {
	offsets := make(map[string]float64)
	if raw, ok := prevState["file_offsets"]; ok {
		switch m := raw.(type) {
		case map[string]any:
			for k, v := range m {
				if f, ok := v.(float64); ok {
					offsets[k] = f
				}
			}
		case map[string]float64:
			for k, v := range m {
				offsets[k] = v
			}
		}
	}
	return offsets
}

func restoreKnownFiles(prevState map[string]any) map[string]bool {
	known := make(map[string]bool)
	if raw, ok := prevState["known_files"]; ok {
		switch m := raw.(type) {
		case map[string]any:
			for k, v := range m {
				if b, ok := v.(bool); ok && b {
					known[k] = true
				}
			}
		case map[string]bool:
			for k, v := range m {
				if v {
					known[k] = true
				}
			}
		}
	}
	return known
}

func findJSONLFiles(baseDir string, lookback time.Duration) ([]string, error) {
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			if time.Since(info.ModTime()) < lookback {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}

func resolveClaudeProjectJSONL(cliSessionID, originCWD, cwd, projectsDir string) string {
	filename := cliSessionID + ".jsonl"

	// Try to derive the cwd-hash directory from the working directory.
	// Claude Code converts "/" to "-" in the path to create the directory name.
	for _, dir := range []string{originCWD, cwd} {
		if dir == "" {
			continue
		}
		cwdHash := strings.ReplaceAll(dir, "/", "-")
		candidate := filepath.Join(projectsDir, cwdHash, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fallback: search all project directories
	dirs, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, d.Name(), filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return ""
}

func readDesktopMeta(path string) (*desktopSessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta desktopSessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
