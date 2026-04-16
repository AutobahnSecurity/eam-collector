package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func TestValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
  api_key: sk-test-12345
interval: 30
lookback: 48
parsers:
  claude:
    enabled: true
  cursor:
    enabled: false
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	t.Run("server URL", func(t *testing.T) {
		if cfg.Server.URL != "https://eam.example.com" {
			t.Errorf("expected https://eam.example.com, got %s", cfg.Server.URL)
		}
	})

	t.Run("API key", func(t *testing.T) {
		if cfg.Server.APIKey != "sk-test-12345" {
			t.Errorf("expected sk-test-12345, got %s", cfg.Server.APIKey)
		}
	})

	t.Run("interval", func(t *testing.T) {
		if cfg.Interval != 30 {
			t.Errorf("expected 30, got %d", cfg.Interval)
		}
	})

	t.Run("lookback", func(t *testing.T) {
		if cfg.Lookback != 48 {
			t.Errorf("expected 48, got %d", cfg.Lookback)
		}
	})

	t.Run("parsers", func(t *testing.T) {
		if !cfg.Parsers["claude"].Enabled {
			t.Error("expected claude parser to be enabled")
		}
		if cfg.Parsers["cursor"].Enabled {
			t.Error("expected cursor parser to be disabled")
		}
	})
}

func TestMissingURL(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  api_key: sk-test-12345
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing server.url, got nil")
	}
	if !strings.Contains(err.Error(), "server.url is required") {
		t.Errorf("expected error about server.url, got: %s", err.Error())
	}
}

func TestMissingAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing api_key, got nil")
	}
	if !strings.Contains(err.Error(), "server.api_key is required") {
		t.Errorf("expected error about server.api_key, got: %s", err.Error())
	}
}

func TestIntervalClamping(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
  api_key: sk-test-12345
interval: 5
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Interval != 10 {
		t.Errorf("expected interval clamped to 10, got %d", cfg.Interval)
	}
}

func TestLookbackDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
  api_key: sk-test-12345
lookback: 0
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Lookback != 24 {
		t.Errorf("expected lookback defaulted to 24, got %d", cfg.Lookback)
	}
}

func TestURLMustHaveScheme(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: not-a-url
  api_key: sk-test-12345
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for URL without scheme, got nil")
	}
	if !strings.Contains(err.Error(), "http://") && !strings.Contains(err.Error(), "https://") {
		t.Errorf("expected error about scheme, got: %s", err.Error())
	}
}

func TestURLMustHaveHost(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://
  api_key: sk-test-12345
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for URL without hostname, got nil")
	}
	if !strings.Contains(err.Error(), "hostname") {
		t.Errorf("expected error about hostname, got: %s", err.Error())
	}
}

func TestIntervalUpperBound(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
  api_key: sk-test-12345
interval: 86400
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Interval != 3600 {
		t.Errorf("expected interval clamped to 3600, got %d", cfg.Interval)
	}
}

func TestEnvExpansion(t *testing.T) {
	// $HOME is reliably set on macOS/Linux
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME not set")
	}

	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://eam.example.com
  api_key: key-$HOME-suffix
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	expected := "key-" + home + "-suffix"
	if cfg.Server.APIKey != expected {
		t.Errorf("expected %s, got %s", expected, cfg.Server.APIKey)
	}
}
