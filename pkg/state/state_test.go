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
	path := filepath.Join(t.TempDir(), "state.json")
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

func TestSaveCreatesDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "subdir", "state.json")
	store := NewStore(path)

	st := &State{AppliedSHA: "test", ManagedFiles: make(map[string]ManagedFile)}
	require.NoError(t, store.Save(st))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "test", got.AppliedSHA)
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

func TestLoad_CorruptJSON_ReturnsFreshState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("not valid json{{{"), 0o600))

	store := NewStore(path)
	st, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, st.AppliedSHA)
	assert.NotNil(t, st.ManagedFiles)
	assert.Empty(t, st.ManagedFiles)
}

func TestLoad_OldSchemaFormat_ReturnsFreshState(t *testing.T) {
	t.Parallel()
	// Old schema: ManagedFiles was map[string]string
	oldJSON := `{"applied_sha":"abc","managed_files":{"/etc/foo":"sha256:deadbeef"}}`
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte(oldJSON), 0o600))

	store := NewStore(path)
	st, err := store.Load()
	require.NoError(t, err)
	// Old format should cause unmarshal error → fresh state
	assert.Empty(t, st.AppliedSHA)
	assert.NotNil(t, st.ManagedFiles)
	assert.Empty(t, st.ManagedFiles)
}
