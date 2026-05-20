package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMissing(t *testing.T) {
	t.Parallel()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	st, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, st.AppliedSHA)
	assert.NotNil(t, st.ManagedFiles)
}

func TestSaveAndLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "subdir", "state.json")
	store := NewStore(path)

	now := time.Now().Truncate(time.Second)
	want := &State{
		AppliedSHA: "abc123",
		AppliedAt:  now,
		ManagedFiles: map[string]ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:deadbeef", Category: "container"},
		},
	}

	require.NoError(t, store.Save(want))

	got, err := store.Load()
	require.NoError(t, err)

	assert.Equal(t, want.AppliedSHA, got.AppliedSHA)
	assert.True(t, got.AppliedAt.Equal(want.AppliedAt))
	require.Len(t, got.ManagedFiles, 1)
	assert.Equal(t, ManagedFile{Hash: "sha256:deadbeef", Category: "container"}, got.ManagedFiles["/etc/containers/systemd/foo.container"])
}

func TestSaveRoundtripWithFailedSHA(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	failedAt := time.Now().Truncate(time.Second)
	st := &State{
		AppliedSHA:   "abc",
		FailedSHA:    "def",
		FailedCount:  2,
		FailedAt:     failedAt,
		ManagedFiles: make(map[string]ManagedFile),
	}
	require.NoError(t, store.Save(st))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "def", got.FailedSHA)
	assert.Equal(t, 2, got.FailedCount)
	assert.True(t, got.FailedAt.Equal(failedAt))
}

func TestLoad_StateWithoutPendingUnits_RoundTrips(t *testing.T) {
	t.Parallel()
	// A state file written before the pending_units feature existed.
	oldJSON := `{"applied_sha":"abc","managed_files":{},"pending_hooks":{"reload-foo":2}}`
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte(oldJSON), 0o600))

	store := NewStore(path)
	st, err := store.Load()
	require.NoError(t, err)
	assert.Nil(t, st.PendingUnits)
	assert.Equal(t, map[string]int{"reload-foo": 2}, st.PendingHooks)

	// Re-saving must not introduce a pending_units key (omitempty).
	require.NoError(t, store.Save(st))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "pending_units")
}

func TestSaveAndLoadPendingUnits(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	first := time.Now().Add(-time.Hour).Truncate(time.Second)
	last := time.Now().Truncate(time.Second)
	st := &State{
		AppliedSHA:   "abc",
		ManagedFiles: make(map[string]ManagedFile),
		PendingUnits: map[string]PendingUnit{
			"vmalert.service": {SHA: "def456", Attempts: 47, FirstFailedAt: first, LastAttemptAt: last},
		},
	}
	require.NoError(t, store.Save(st))

	got, err := store.Load()
	require.NoError(t, err)
	require.Len(t, got.PendingUnits, 1)
	pu := got.PendingUnits["vmalert.service"]
	assert.Equal(t, "def456", pu.SHA)
	assert.Equal(t, 47, pu.Attempts)
	assert.True(t, pu.FirstFailedAt.Equal(first))
	assert.True(t, pu.LastAttemptAt.Equal(last))
}

func TestMarkAppliedIncompleteLeavesLastSuccessful(t *testing.T) {
	t.Parallel()
	st := NewState()

	st.MarkApplied("sha-1")
	converged := st.LastSuccessfulReconciliationAt
	require.False(t, converged.IsZero(), "MarkApplied advances the last-successful timestamp")

	st.MarkAppliedIncomplete("sha-2")
	assert.Equal(t, "sha-2", st.AppliedSHA, "incomplete apply still records the SHA")
	assert.Equal(t, 0, st.FailedCount, "incomplete apply still clears failure tracking")
	assert.True(t, st.LastSuccessfulReconciliationAt.Equal(converged),
		"incomplete apply must not advance the last-successful timestamp")
}

func TestLoad_CorruptJSON_ReturnsErrCorrupt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("not valid json{{{"), 0o600))

	store := NewStore(path)
	st, err := store.Load()
	require.ErrorIs(t, err, ErrCorrupt)
	assert.Nil(t, st)
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("not valid json{{{"), data)
}

func TestLoad_OldSchemaFormat_ReturnsErrCorrupt(t *testing.T) {
	t.Parallel()
	// Old schema: ManagedFiles was map[string]string
	oldJSON := `{"applied_sha":"abc","managed_files":{"/etc/foo":"sha256:deadbeef"}}`
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte(oldJSON), 0o600))

	store := NewStore(path)
	st, err := store.Load()
	require.ErrorIs(t, err, ErrCorrupt)
	assert.Nil(t, st)
}
