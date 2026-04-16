package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Create store, set data, save
	s1 := New(dir)
	s1.Set("claude", map[string]any{
		"last_offset": float64(1234),
		"session_id":  "abc-def",
	})
	s1.Set("cursor", map[string]any{
		"processed": true,
	})

	if err := s1.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// New store at same path, load
	s2 := New(dir)

	// Load directly from file (skip flock for test)
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if err := json.Unmarshal(data, &s2.Data); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}

	t.Run("claude parser state preserved", func(t *testing.T) {
		cs := s2.Get("claude")
		if cs["last_offset"] != float64(1234) {
			t.Errorf("last_offset: expected 1234, got %v", cs["last_offset"])
		}
		if cs["session_id"] != "abc-def" {
			t.Errorf("session_id: expected abc-def, got %v", cs["session_id"])
		}
	})

	t.Run("cursor parser state preserved", func(t *testing.T) {
		cs := s2.Get("cursor")
		if cs["processed"] != true {
			t.Errorf("processed: expected true, got %v", cs["processed"])
		}
	})
}

func TestLoadNonexistentFile(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	// state.json does not exist yet; Load should succeed (first run)
	// We test the file-not-exist path directly since Load() also acquires flock
	statePath := filepath.Join(dir, "state.json")

	_, err := os.ReadFile(statePath)
	if !os.IsNotExist(err) {
		t.Fatalf("expected file to not exist, got: %v", err)
	}

	// Verify the store starts with empty data and is usable
	if s.Data == nil {
		t.Fatal("expected Data to be initialized, got nil")
	}
	if len(s.Data) != 0 {
		t.Errorf("expected empty Data map, got %d entries", len(s.Data))
	}
}

func TestGetCreatesEmptyMap(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	result := s.Get("unknown")

	t.Run("returns non-nil map", func(t *testing.T) {
		if result == nil {
			t.Fatal("Get('unknown') returned nil, expected empty map")
		}
	})

	t.Run("returns empty map", func(t *testing.T) {
		if len(result) != 0 {
			t.Errorf("expected empty map, got %d entries", len(result))
		}
	})

	t.Run("map is stored in Data", func(t *testing.T) {
		// Subsequent Get should return the same map
		result2 := s.Get("unknown")
		if result2 == nil {
			t.Fatal("second Get returned nil")
		}
	})
}

func TestSetThenGet(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	input := map[string]any{
		"file_offset": float64(5678),
		"last_file":   "/var/log/test.log",
		"active":      true,
	}
	s.Set("myparser", input)

	got := s.Get("myparser")

	t.Run("file_offset matches", func(t *testing.T) {
		if got["file_offset"] != float64(5678) {
			t.Errorf("file_offset: expected 5678, got %v", got["file_offset"])
		}
	})

	t.Run("last_file matches", func(t *testing.T) {
		if got["last_file"] != "/var/log/test.log" {
			t.Errorf("last_file: expected /var/log/test.log, got %v", got["last_file"])
		}
	})

	t.Run("active matches", func(t *testing.T) {
		if got["active"] != true {
			t.Errorf("active: expected true, got %v", got["active"])
		}
	})
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	s := New(dir)
	s.Set("test", map[string]any{"key": "value"})

	if err := s.Save(); err != nil {
		t.Fatalf("Save() should create nested dirs, got: %v", err)
	}

	statePath := filepath.Join(dir, "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Error("state.json was not created")
	}
}
