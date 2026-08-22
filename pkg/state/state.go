package state

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/schjan/picolet/internal/atomicfile"
	"github.com/schjan/picolet/pkg/config"
)

// ErrCorrupt is returned when the state file exists but cannot be decoded.
var ErrCorrupt = errors.New("state file corrupt")

// ManagedFile tracks a picolet-owned file's content hash and category.
type ManagedFile struct {
	Hash     string          `json:"hash"`
	Category config.Category `json:"category"`
}

// PendingUnit records a managed systemd unit whose last restart attempt failed.
// Persisted so the failure (and its retry cooldown) survives an agent restart.
type PendingUnit struct {
	SHA           string    `json:"sha"`             // git SHA in effect when this failure was last recorded
	Attempts      int       `json:"attempts"`        // consecutive failed restart attempts
	FirstFailedAt time.Time `json:"first_failed_at"` // when the unit first failed to restart
	LastAttemptAt time.Time `json:"last_attempt_at"` // most recent restart attempt; backs the retry cooldown
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

	// LastPrunedAt records the last time unused images were pruned. Drives the
	// prune-interval gate and survives restarts so a node does not re-prune on
	// every boot. omitzero (not omitempty — time.Time is never "empty").
	LastPrunedAt time.Time `json:"last_pruned_at,omitzero"`

	// PendingHooks tracks hooks that errored under on_failure: keep_running
	// and must retry on the next reconciliation tick. The map value is the number
	// of consecutive failed attempts. Persisted so a restart does not silently
	// abandon a promised retry.
	// Renamed from pending_secret_hooks (unreleased feature, no migration needed).
	PendingHooks map[string]int `json:"pending_hooks,omitempty"`

	// PendingUnits tracks managed systemd units whose last restart attempt
	// failed and that must keep being retried. Persisted so a restart does not
	// erase the failure record, the retry cooldown, or the attempt count.
	// Additive omitempty field: a state file written before this feature
	// decodes with PendingUnits nil, which is safe (len(nil) == 0).
	PendingUnits map[string]PendingUnit `json:"pending_units,omitempty"`
}

// NewState returns a zero State with initialized maps, suitable for first-run or testing.
func NewState() *State {
	return &State{
		ManagedFiles: make(map[string]ManagedFile),
		ServiceNames: make(map[string]string),
	}
}

// markApplied records the SHA and clears failure tracking. It does not touch
// the last-successful timestamp; callers decide whether the reconciliation
// fully converged.
func (s *State) markApplied(headSHA string) {
	s.AppliedSHA = headSHA
	s.AppliedAt = time.Now()
	s.FailedSHA = ""
	s.FailedCount = 0
	s.FailedAt = time.Time{}
}

// MarkApplied records the SHA as a fully successful reconciliation, advancing
// the last-successful timestamp.
func (s *State) MarkApplied(headSHA string) {
	s.markApplied(headSHA)
	s.LastSuccessfulReconciliationAt = s.AppliedAt
}

// MarkAppliedIncomplete records the SHA after an apply that did not fully
// converge (a keep_running hook or a unit restart is still failing). The SHA is
// recorded so gitpoll stops reporting "Changed", but the last-successful
// timestamp is NOT advanced — the fleet has not converged.
func (s *State) MarkAppliedIncomplete(headSHA string) {
	s.markApplied(headSHA)
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
	data, err := json.Marshal(st, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	return atomicfile.WriteFile(s.path, data, 0o600)
}
