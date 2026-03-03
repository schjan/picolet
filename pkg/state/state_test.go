package state

import (
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
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:deadbeef",
		},
	}

	require.NoError(t, store.Save(want))

	got, err := store.Load()
	require.NoError(t, err)

	assert.Equal(t, want.AppliedSHA, got.AppliedSHA)
	assert.True(t, got.AppliedAt.Equal(want.AppliedAt))
	require.Len(t, got.ManagedFiles, 1)
	assert.Equal(t, "sha256:deadbeef", got.ManagedFiles["/etc/containers/systemd/foo.container"])
}

func TestSaveCreatesDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "subdir", "state.json")
	store := NewStore(path)

	st := &State{AppliedSHA: "test", ManagedFiles: make(map[string]string)}
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
		ManagedFiles: make(map[string]string),
	}
	require.NoError(t, store.Save(st))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "def", got.FailedSHA)
	assert.Equal(t, 2, got.FailedCount)
	assert.True(t, got.FailedAt.Equal(failedAt))
}
