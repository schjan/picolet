package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State represents the persisted reconciliation state.
type State struct {
	AppliedSHA   string            `json:"applied_sha"`
	AppliedAt    time.Time         `json:"applied_at"`
	ManagedFiles map[string]string `json:"managed_files"` // destPath → "sha256:..."
	FailedSHA    string            `json:"failed_sha"`
	FailedCount  int               `json:"failed_count"`
	FailedAt     time.Time         `json:"failed_at"`
}

// MarkApplied resets failure tracking and records the SHA as successfully applied.
func (s *State) MarkApplied(headSHA string) {
	s.AppliedSHA = headSHA
	s.AppliedAt = time.Now()
	s.FailedSHA = ""
	s.FailedCount = 0
	s.FailedAt = time.Time{}
}

// Store manages atomic reads and writes of the state file.
type Store struct {
	path string
}

// NewStore creates a new state store at the given path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Load reads the state from disk. Returns a zero State if the file does not exist (first run).
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{ManagedFiles: make(map[string]string)}, nil
		}
		return nil, fmt.Errorf("reading state %s: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing state %s: %w", s.path, err)
	}
	if st.ManagedFiles == nil {
		st.ManagedFiles = make(map[string]string)
	}
	return &st, nil
}

// Save writes the state atomically using tmp + rename.
func (s *Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing temp state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming state file: %w", err)
	}
	return nil
}
