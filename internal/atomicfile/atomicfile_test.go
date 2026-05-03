package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/atomicfile"
)

func TestWriteFileCreatesAndReplacesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, atomicfile.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, atomicfile.WriteFile(path, []byte("new"), 0o640))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), data)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	assertNoTemps(t, dir, "state.json.tmp-*")
}

func TestWriteFileRemovesTempFileOnRenameError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(path, 0o755))

	err := atomicfile.WriteFile(path, []byte("data"), 0o600)
	require.Error(t, err)
	assertNoTemps(t, dir, "target.tmp-*")
}

func assertNoTemps(t *testing.T, dir, pattern string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
