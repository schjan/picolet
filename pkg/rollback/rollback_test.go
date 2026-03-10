package rollback

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

func TestCreateAndRestore(t *testing.T) {
	t.Parallel()
	sys := applier.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)

	written := make(map[string][]byte)
	removed := []string{}
	fw := applier.NewMockFileWriter(t)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		written[path] = content
		return nil
	})
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).RunAndReturn(func(path string) error {
		removed = append(removed, path)
		return nil
	})

	mgr := New(fw, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/new.container", Action: reconciler.ActionCreate, Category: "container"},
			{DestPath: "/etc/containers/systemd/old.container", Action: reconciler.ActionUpdate, Category: "container"},
			{DestPath: "secret:my_secret", Action: reconciler.ActionCreate, Category: "secret"},
		},
	}

	// Mock disk reader: old.container exists, new.container does not
	diskReader := func(path string) ([]byte, error) {
		if path == "/etc/containers/systemd/old.container" {
			return []byte("original-content"), nil
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	snap, err := mgr.Create(cs, diskReader)
	require.NoError(t, err)

	// Secret should be skipped
	assert.NotContains(t, snap.Files, "secret:my_secret", "secret should be skipped in snapshot")

	// new.container → nil (didn't exist)
	content, ok := snap.Files["/etc/containers/systemd/new.container"]
	assert.True(t, ok, "new.container should be in snapshot")
	assert.Nil(t, content, "new.container should be nil in snapshot")

	// old.container → original content
	assert.Equal(t, []byte("original-content"), snap.Files["/etc/containers/systemd/old.container"])

	// Now restore
	require.NoError(t, mgr.Restore(context.Background(), snap))

	// new.container should be removed
	assert.Equal(t, []string{"/etc/containers/systemd/new.container"}, removed)

	// old.container should be restored
	assert.Equal(t, []byte("original-content"), written["/etc/containers/systemd/old.container"])
}

func TestSnapshotWithRealFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "existing.conf")
	require.NoError(t, os.WriteFile(existingPath, []byte("existing"), 0o600))

	sys := applier.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	fw := applier.NewMockFileWriter(t)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()

	mgr := New(fw, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: existingPath, Action: reconciler.ActionUpdate, Category: "container"},
			{DestPath: filepath.Join(dir, "new.conf"), Action: reconciler.ActionCreate, Category: "network"},
		},
	}

	snap, err := mgr.Create(cs, os.ReadFile)
	require.NoError(t, err)

	assert.Equal(t, []byte("existing"), snap.Files[existingPath])
	assert.Nil(t, snap.Files[filepath.Join(dir, "new.conf")])

	require.NoError(t, mgr.Restore(context.Background(), snap))
}

func TestCreateSkipsNoop(t *testing.T) {
	t.Parallel()
	sys := applier.NewMockSystemdManager(t)
	fw := applier.NewMockFileWriter(t)
	// No expectations — noop should not trigger any calls
	mgr := New(fw, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/noop.container", Action: reconciler.ActionNoop, Category: "container"},
		},
	}

	snap, err := mgr.Create(cs, nil)
	require.NoError(t, err)
	assert.Empty(t, snap.Files)
}
