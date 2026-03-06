package orphan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/orphan"
)

func TestScanOwnedDir_RemovesOrphans(t *testing.T) {
	t.Parallel()
	quadletDir := t.TempDir()

	// Write an orphan (not in managedFiles)
	orphanPath := filepath.Join(quadletDir, "old.container")
	require.NoError(t, os.WriteFile(orphanPath, []byte("[Container]"), 0o600))

	// Write a managed file (in managedFiles)
	managedPath := filepath.Join(quadletDir, "current.container")
	require.NoError(t, os.WriteFile(managedPath, []byte("[Container]"), 0o600))

	fw := mocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, quadletDir, t.TempDir(), t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{
		managedPath: "sha256:abc",
	}))
}

func TestScanOwnedDir_DirNotExist(t *testing.T) {
	t.Parallel()
	fw := mocks.NewMockFileWriter(t)
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	s := orphan.New(fw, pod, nonExistent, t.TempDir(), t.TempDir())
	// Should return nil — non-existent dir means no orphans
	require.NoError(t, s.Scan(context.Background(), map[string]string{}))
}

func TestScanMarkedDir_RemovesOrphans(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	// Write an orphan with the picolet marker
	orphanPath := filepath.Join(systemdDir, "old.service")
	content := applier.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(orphanPath, []byte(content), 0o600))

	fw := mocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{}))
}

func TestScanMarkedDir_IgnoresUnmarkedFiles(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	// File without the picolet marker — must NOT be touched
	require.NoError(t, os.WriteFile(
		filepath.Join(systemdDir, "foreign.service"),
		[]byte("[Service]\nExecStart=/usr/bin/myapp\n"),
		0o600,
	))

	fw := mocks.NewMockFileWriter(t)
	// No Remove calls expected
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{}))
}

func TestScanMarkedDir_KeepsManagedFiles(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	managedPath := filepath.Join(systemdDir, "managed.service")
	content := applier.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(managedPath, []byte(content), 0o600))

	fw := mocks.NewMockFileWriter(t)
	// No Remove calls expected — file is in managedFiles
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{
		managedPath: "sha256:abc",
	}))
}

func TestScanSecrets_RemovesOrphans(t *testing.T) {
	t.Parallel()
	fw := mocks.NewMockFileWriter(t)
	pod := mocks.NewMockPodmanClient(t)
	// "kept" is in state, "orphan" is not
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"kept", "orphan"}, nil)
	pod.EXPECT().SecretRemove(mock.Anything, "orphan").Return(nil)

	s := orphan.New(fw, pod, t.TempDir(), t.TempDir(), t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{
		"secret:kept": "sha256:abc",
	}))
}

func TestScanSecrets_KeepsAllManaged(t *testing.T) {
	t.Parallel()
	fw := mocks.NewMockFileWriter(t)
	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"db-pass", "api-key"}, nil)
	// No SecretRemove calls expected

	s := orphan.New(fw, pod, t.TempDir(), t.TempDir(), t.TempDir())
	require.NoError(t, s.Scan(context.Background(), map[string]string{
		"secret:db-pass": "sha256:abc",
		"secret:api-key": "sha256:def",
	}))
}
