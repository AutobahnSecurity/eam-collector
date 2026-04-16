package parsers

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// desktopDir mirrors platform.ClaudeDesktopDir logic for the test home.
func desktopDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude")
	default:
		return filepath.Join(home, ".config", "Claude")
	}
}

// --- readStatsigIdentity tests ---

func TestReadStatsigIdentity_ValidFile(t *testing.T) {
	home := t.TempDir()
	statsigDir := filepath.Join(home, ".claude", "statsig")
	if err := os.MkdirAll(statsigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `{"data": "{\"evaluated_keys\":{\"customIDs\":{\"accountUUID\":\"acc-uuid-1234\",\"organizationUUID\":\"org-uuid-5678\"}}}"}`
	if err := os.WriteFile(filepath.Join(statsigDir, "statsig.cached.evaluations.abc123"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	id := readStatsigIdentity(home)
	if id == nil {
		t.Fatal("readStatsigIdentity returned nil, want non-nil")
	}
	if id.AccountUUID != "acc-uuid-1234" {
		t.Errorf("AccountUUID = %q, want %q", id.AccountUUID, "acc-uuid-1234")
	}
	if id.OrganizationUUID != "org-uuid-5678" {
		t.Errorf("OrganizationUUID = %q, want %q", id.OrganizationUUID, "org-uuid-5678")
	}
	if id.Tool != "claude-code" {
		t.Errorf("Tool = %q, want %q", id.Tool, "claude-code")
	}
}

func TestReadStatsigIdentity_NoFiles(t *testing.T) {
	home := t.TempDir()
	statsigDir := filepath.Join(home, ".claude", "statsig")
	if err := os.MkdirAll(statsigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id := readStatsigIdentity(home)
	if id != nil {
		t.Errorf("readStatsigIdentity empty dir = %+v, want nil", id)
	}
}

func TestReadStatsigIdentity_EmptyAccountUUID(t *testing.T) {
	home := t.TempDir()
	statsigDir := filepath.Join(home, ".claude", "statsig")
	if err := os.MkdirAll(statsigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `{"data": "{\"evaluated_keys\":{\"customIDs\":{\"accountUUID\":\"\",\"organizationUUID\":\"org-uuid-5678\"}}}"}`
	if err := os.WriteFile(filepath.Join(statsigDir, "statsig.cached.evaluations.xyz"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	id := readStatsigIdentity(home)
	if id != nil {
		t.Errorf("readStatsigIdentity empty accountUUID = %+v, want nil", id)
	}
}

func TestReadStatsigIdentity_PicksNewestFile(t *testing.T) {
	home := t.TempDir()
	statsigDir := filepath.Join(home, ".claude", "statsig")
	if err := os.MkdirAll(statsigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldContent := `{"data": "{\"evaluated_keys\":{\"customIDs\":{\"accountUUID\":\"old-account\",\"organizationUUID\":\"old-org\"}}}"}`
	oldFile := filepath.Join(statsigDir, "statsig.cached.evaluations.old")
	if err := os.WriteFile(oldFile, []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set old mtime
	oldTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	newContent := `{"data": "{\"evaluated_keys\":{\"customIDs\":{\"accountUUID\":\"new-account\",\"organizationUUID\":\"new-org\"}}}"}`
	newFile := filepath.Join(statsigDir, "statsig.cached.evaluations.new")
	if err := os.WriteFile(newFile, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}

	id := readStatsigIdentity(home)
	if id == nil {
		t.Fatal("readStatsigIdentity returned nil, want non-nil")
	}
	if id.AccountUUID != "new-account" {
		t.Errorf("AccountUUID = %q, want %q (newest file)", id.AccountUUID, "new-account")
	}
	if id.OrganizationUUID != "new-org" {
		t.Errorf("OrganizationUUID = %q, want %q (newest file)", id.OrganizationUUID, "new-org")
	}
}

// --- readDesktopIdentity tests ---

func TestReadDesktopIdentity_ValidDirs(t *testing.T) {
	home := t.TempDir()
	appDir := desktopDir(home)
	sessDir := filepath.Join(appDir, "local-agent-mode-sessions",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id := readDesktopIdentity(home)
	if id == nil {
		t.Fatal("readDesktopIdentity returned nil, want non-nil")
	}
	if id.AccountUUID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("AccountUUID = %q, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", id.AccountUUID)
	}
	if id.OrganizationUUID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("OrganizationUUID = %q, want 11111111-2222-3333-4444-555555555555", id.OrganizationUUID)
	}
	if id.Tool != "claude-desktop" {
		t.Errorf("Tool = %q, want %q", id.Tool, "claude-desktop")
	}
}

func TestReadDesktopIdentity_SkipsNonUUID(t *testing.T) {
	home := t.TempDir()
	appDir := desktopDir(home)
	sessionsBase := filepath.Join(appDir, "local-agent-mode-sessions")

	// Create a non-UUID directory -- should be skipped
	nonUUIDDir := filepath.Join(sessionsBase, "not-a-uuid", "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(nonUUIDDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a valid UUID directory
	validDir := filepath.Join(sessionsBase,
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(validDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id := readDesktopIdentity(home)
	if id == nil {
		t.Fatal("readDesktopIdentity returned nil, want non-nil")
	}
	if id.AccountUUID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("AccountUUID = %q, want valid UUID (not-a-uuid should be skipped)", id.AccountUUID)
	}
}

func TestReadDesktopIdentity_SkipsSkillsPlugin(t *testing.T) {
	home := t.TempDir()
	appDir := desktopDir(home)
	sessionsBase := filepath.Join(appDir, "local-agent-mode-sessions")

	// Create skills-plugin directory -- should be skipped
	skillsDir := filepath.Join(sessionsBase, "skills-plugin", "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id := readDesktopIdentity(home)
	if id != nil {
		t.Errorf("readDesktopIdentity skills-plugin = %+v, want nil", id)
	}
}

func TestReadDesktopIdentity_EmptyDirs(t *testing.T) {
	home := t.TempDir()
	appDir := desktopDir(home)
	sessionsBase := filepath.Join(appDir, "local-agent-mode-sessions")
	if err := os.MkdirAll(sessionsBase, 0o755); err != nil {
		t.Fatal(err)
	}

	id := readDesktopIdentity(home)
	if id != nil {
		t.Errorf("readDesktopIdentity empty = %+v, want nil", id)
	}
}
