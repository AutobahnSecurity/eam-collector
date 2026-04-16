package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// HomeDir returns the current user's home directory or a descriptive error.
// All callers should use this instead of os.UserHomeDir() directly to ensure
// consistent error handling and messaging.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return home, nil
}

// ClaudeDesktopDir returns the Claude Desktop application data directory.
// Supports darwin, windows, and linux.
func ClaudeDesktopDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude")
	default:
		return filepath.Join(home, ".config", "Claude")
	}
}

// ClaudeDesktopCacheDir returns the Chrome HTTP cache directory for Claude Desktop.
func ClaudeDesktopCacheDir(home string) string {
	return filepath.Join(ClaudeDesktopDir(home), "Cache", "Cache_Data")
}

// ClaudeDesktopLDBDir returns the Local Storage LevelDB directory for Claude Desktop.
func ClaudeDesktopLDBDir(home string) string {
	return filepath.Join(ClaudeDesktopDir(home), "Local Storage", "leveldb")
}

// CursorDBPath returns the path to Cursor's SQLite state database.
func CursorDBPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}

// CopilotBaseDir returns the VS Code workspace storage directory for Copilot sessions.
func CopilotBaseDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "workspaceStorage")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "workspaceStorage")
	default:
		return filepath.Join(home, ".config", "Code", "User", "workspaceStorage")
	}
}

// ContinueSessionsDir returns the Continue.dev sessions directory.
func ContinueSessionsDir(home string) string {
	return filepath.Join(home, ".continue", "sessions")
}
