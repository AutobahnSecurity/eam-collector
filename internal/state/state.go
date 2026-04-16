package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists per-parser state (file offsets, processed IDs, etc.)
// so the collector only ships new data on each run.
// The mutex protects Data for goroutine safety if parsers are ever
// collected concurrently.
type Store struct {
	mu   sync.Mutex
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

	// Acquire file lock to prevent concurrent collector instances
	lockPath := s.path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	if err := acquireLock(f); err != nil {
		f.Close()
		return fmt.Errorf("another eam-collector instance is already running (lock: %s)", lockPath)
	}
	s.lock = f

	// Write our PID to the lock file so --stop/--status can find us
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Sync()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// Close releases the file lock.
func (s *Store) Close() {
	if s.lock != nil {
		releaseLock(s.lock)
		s.lock.Close()
		s.lock = nil
	}
}

func (s *Store) Get(parser string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Data[parser] == nil {
		s.Data[parser] = make(map[string]any)
	}
	return s.Data[parser]
}

func (s *Store) Set(parser string, state map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Data[parser] = state
}

// LockPath returns the path to the lock file for this store.
func (s *Store) LockPath() string {
	return s.path + ".lock"
}

// ReadPID reads the PID of the running collector from the lock file.
// Returns 0 if no PID is found or the file doesn't exist.
func ReadPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	return pid
}
