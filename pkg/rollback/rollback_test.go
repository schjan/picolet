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

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

func TestCreateAndRestore(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)

	written := make(map[string][]byte)
	removed := []string{}
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		written[path] = content
		return nil
	})
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).RunAndReturn(func(path string) error {
		removed = append(removed, path)
		return nil
	})

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

	snap, err := CreateSnapshot(cs, diskReader)
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
	require.NoError(t, Restore(context.Background(), snap, fw, sys))

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

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: existingPath, Action: reconciler.ActionUpdate, Category: "container"},
			{DestPath: filepath.Join(dir, "new.conf"), Action: reconciler.ActionCreate, Category: "network"},
		},
	}

	snap, err := CreateSnapshot(cs, os.ReadFile)
	require.NoError(t, err)

	assert.Equal(t, []byte("existing"), snap.Files[existingPath])
	assert.Nil(t, snap.Files[filepath.Join(dir, "new.conf")])

	require.NoError(t, Restore(context.Background(), snap, fw, sys))
}

func TestCreateSkipsNoop(t *testing.T) {
	t.Parallel()
	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/noop.container", Action: reconciler.ActionNoop, Category: "container"},
		},
	}

	snap, err := CreateSnapshot(cs, os.ReadFile)
	require.NoError(t, err)
	assert.Empty(t, snap.Files)
}

func TestCreateSnapshotNilReaderReturnsError(t *testing.T) {
	t.Parallel()

	snap, err := CreateSnapshot(&reconciler.Changeset{}, nil)
	require.ErrorContains(t, err, "disk reader is nil")
	assert.Nil(t, snap)
}

// updateChangeset returns a changeset updating one on-disk file, created under
// a temp dir so CreateSnapshot's os.ReadFile sees real prior content.
func updateChangeset(t *testing.T, oldContent, newContent string) (*reconciler.Changeset, string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "app.container")
	require.NoError(t, os.WriteFile(dest, []byte(oldContent), 0o600))
	return &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:   dest,
			Action:     reconciler.ActionUpdate,
			Category:   "container",
			NewContent: newContent,
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, dest
}

func TestApplyWithSnapshotSuccessDoesNotRestore(t *testing.T) {
	t.Parallel()
	cs, dest := updateChangeset(t, "old", "new")

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().WriteFile(dest, []byte("new")).Return(nil).Once()

	app := applier.New(sys, pod, fw, false, nil)
	result, rolledBack, err := ApplyWithSnapshot(context.Background(), app, cs, nil, fw, sys)
	require.NoError(t, err)
	assert.False(t, rolledBack)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.Applied)
}

func TestApplyWithSnapshotRestoresOnFatalError(t *testing.T) {
	t.Parallel()
	cs, dest := updateChangeset(t, "old", "new")

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil) // restore's daemon-reload
	pod := appliermocks.NewMockPodmanClient(t)
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	// The apply's write fails fatally; the restore then writes the snapshot back.
	fw.EXPECT().WriteFile(dest, []byte("new")).Return(fmt.Errorf("disk full")).Once()
	fw.EXPECT().WriteFile(dest, []byte("old")).Return(nil).Once()

	app := applier.New(sys, pod, fw, false, nil)
	result, rolledBack, err := ApplyWithSnapshot(context.Background(), app, cs, nil, fw, sys)
	require.ErrorContains(t, err, "disk full")
	assert.True(t, rolledBack)
	assert.Nil(t, result)
}
