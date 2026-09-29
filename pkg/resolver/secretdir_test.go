package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirSecretReaderConfinedToDir(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host.key"), []byte("HOST-KEY"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api.token"), []byte("token"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "host.key"), filepath.Join(dir, "link.key")))

	read, release := DirSecretReader(dir)
	defer release()

	for range 2 { // a reader serves every secret of a resolve pass
		got, err := read("api.token")
		require.NoError(t, err)
		assert.Equal(t, "token", got)
	}
	for _, name := range []string{"../" + filepath.Base(outside) + "/host.key", "link.key"} {
		got, err := read(name)
		require.Error(t, err, name)
		assert.Empty(t, got, name)
	}
}

func TestDirSecretReaderMissingDirOnlyFailsOnRead(t *testing.T) {
	t.Parallel()
	read, release := DirSecretReader(filepath.Join(t.TempDir(), "absent"))
	defer release() // nothing opened: must not panic

	_, err := read("api.token")
	require.ErrorContains(t, err, "opening secrets dir")
}
