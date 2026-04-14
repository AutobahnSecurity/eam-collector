package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Store persists per-parser state (file offsets, processed IDs, etc.)
// so the collector only ships new data on each run.
type Store struct {
	path string
	Data map[string]map[string]any `json:"parsers"`
	lock *os.File // flock handle
}

func New(dir string) *Store {
	return &Store{
		path: filepath.Join(dir, "state.json"),
		Data: make(map[string]map[string]any),
	}
}

func (s *Store) Load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}

	// Acquire file lock to prevent concurrent collector instances.
	// Retry a few times with short delays — on restart, the old process
	// may still be exiting when launchd starts the new one.
	// NOTE: syscall.Flock is Unix-only (darwin/linux). This collector
	// currently targets macOS (Homebrew). Windows support would need
	// LockFileEx via golang.org/x/sys/windows or a cross-platform lib.
	lockPath := s.path + ".lock"
	var f *os.File
	for attempt := 0; attempt < 5; attempt++ {
		var err error
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return fmt.Errorf("open lock file: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if attempt < 4 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return fmt.Errorf("another eam-collector instance is already running (lock: %s)", lockPath)
		}
		break
	}
	s.lock = f

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run
		}
		return err
	}
	return json.Unmarshal(data, &s.Data)
}

// Save writes state atomically (write to temp file, then rename).
func (s *Store) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Data, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write: write to temp, then rename
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// Close releases the file lock and removes the lock file.
func (s *Store) Close() {
	if s.lock != nil {
		lockPath := s.lock.Name()
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
		os.Remove(lockPath)
	}
}

func (s *Store) Get(parser string) map[string]any {
	if s.Data[parser] == nil {
		s.Data[parser] = make(map[string]any)
	}
	return s.Data[parser]
}

func (s *Store) Set(parser string, state map[string]any) {
	s.Data[parser] = state
}
