package orphan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/orphan"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
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

	fw := applier.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, quadletDir, t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		managedPath: {Hash: "sha256:abc", Category: "container"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesRemoved)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScanOwnedDir_DirNotExist(t *testing.T) {
	t.Parallel()
	fw := applier.NewMockFileWriter(t)
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	s := orphan.New(fw, pod, nonExistent, t.TempDir(), t.TempDir())
	// Should return zero — non-existent dir means no orphans
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScanMarkedDir_RemovesOrphans(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	// Write an orphan with the picolet marker
	orphanPath := filepath.Join(systemdDir, "old.service")
	content := resolver.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(orphanPath, []byte(content), 0o600))

	fw := applier.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesRemoved)
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

	fw := applier.NewMockFileWriter(t)
	// No Remove calls expected
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
}

func TestScanMarkedDir_KeepsManagedFiles(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	managedPath := filepath.Join(systemdDir, "managed.service")
	content := resolver.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(managedPath, []byte(content), 0o600))

	fw := applier.NewMockFileWriter(t)
	// No Remove calls expected — file is in managedFiles
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, t.TempDir(), systemdDir, t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		managedPath: {Hash: "sha256:abc", Category: "systemd"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
}

func TestScanSecrets_RemovesOrphans(t *testing.T) {
	t.Parallel()
	fw := applier.NewMockFileWriter(t)
	pod := applier.NewMockPodmanClient(t)
	// "kept" is in state, "orphan" is not
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"kept", "orphan"}, nil)
	pod.EXPECT().SecretRemove(mock.Anything, "orphan").Return(nil)

	s := orphan.New(fw, pod, t.TempDir(), t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		"secret:kept": {Hash: "sha256:abc", Category: "secret"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.SecretsRemoved)
}

func TestScanSecrets_KeepsAllManaged(t *testing.T) {
	t.Parallel()
	fw := applier.NewMockFileWriter(t)
	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"db-pass", "api-key"}, nil)
	// No SecretRemove calls expected

	s := orphan.New(fw, pod, t.TempDir(), t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		"secret:db-pass": {Hash: "sha256:abc", Category: "secret"},
		"secret:api-key": {Hash: "sha256:def", Category: "secret"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScan_CountsFilesAndSecretsSeparately(t *testing.T) {
	t.Parallel()
	quadletDir := t.TempDir()

	// Write 2 orphan files
	require.NoError(t, os.WriteFile(filepath.Join(quadletDir, "a.container"), []byte("[Container]"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(quadletDir, "b.container"), []byte("[Container]"), 0o600))

	fw := applier.NewMockFileWriter(t)
	fw.EXPECT().Remove(mock.Anything).Return(nil).Times(2)

	pod := applier.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"orphan-secret"}, nil)
	pod.EXPECT().SecretRemove(mock.Anything, "orphan-secret").Return(nil)

	s := orphan.New(fw, pod, quadletDir, t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.FilesRemoved)
	assert.Equal(t, 1, result.SecretsRemoved)
}
