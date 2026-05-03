package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/schjan/picolet/internal/atomicfile"
)

// ErrCorrupt is returned when the state file exists but cannot be decoded.
var ErrCorrupt = errors.New("state file corrupt")

// ManagedFile tracks a picolet-owned file's content hash and category.
type ManagedFile struct {
	Hash     string `json:"hash"`
	Category string `json:"category"`
}

// State represents the persisted reconciliation state.
type State struct {
	AppliedSHA   string                 `json:"applied_sha"`
	AppliedAt    time.Time              `json:"applied_at"`
	ManagedFiles map[string]ManagedFile `json:"managed_files"` // destPath → {hash, category}
	ServiceNames map[string]string      `json:"service_names"` // destPath → "foo.service" for quadlets
	FailedSHA    string                 `json:"failed_sha"`
	FailedCount  int                    `json:"failed_count"`
	FailedAt     time.Time              `json:"failed_at"`

	LastSuccessfulReconciliationAt time.Time `json:"last_successful_reconciliation_at,omitzero"`
}

// NewState returns a zero State with initialized maps, suitable for first-run or testing.
func NewState() *State {
	return &State{
		ManagedFiles: make(map[string]ManagedFile),
		ServiceNames: make(map[string]string),
	}
}

// MarkApplied resets failure tracking and records the SHA as successfully applied.
func (s *State) MarkApplied(headSHA string) {
	s.AppliedSHA = headSHA
	s.AppliedAt = time.Now()
	s.LastSuccessfulReconciliationAt = s.AppliedAt
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
			return NewState(), nil
		}
		return nil, fmt.Errorf("reading state %s: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decoding state %s: %w: %w", s.path, ErrCorrupt, err)
	}
	if st.ManagedFiles == nil {
		st.ManagedFiles = make(map[string]ManagedFile)
	}
	if st.ServiceNames == nil {
		st.ServiceNames = make(map[string]string)
	}
	return &st, nil
}

// Save writes the state atomically using a unique temp file + rename.
func (s *Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	return atomicfile.WriteFile(s.path, data, 0o600)
}
