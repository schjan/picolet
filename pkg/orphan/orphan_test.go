package orphan_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/orphan"
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

	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().StopUnit(mock.Anything, "old.service").Return(nil)
	s := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		managedPath: {Hash: "sha256:abc", Category: "container"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesRemoved)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScanOwnedDir_DirNotExist(t *testing.T) {
	t.Parallel()
	fw := appliermocks.NewMockFileWriter(t)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), nonExistent, t.TempDir(), t.TempDir())
	// Should return zero — non-existent dir means no orphans
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScanFilesDir_RemovesOnlyOrphanedFiles(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	filesDir := filepath.Join(dataDir, "files", "config")
	require.NoError(t, os.MkdirAll(filesDir, 0o755))

	orphanPath := filepath.Join(filesDir, "old.yml")
	require.NoError(t, os.WriteFile(orphanPath, []byte("old: true\n"), 0o600))
	managedPath := filepath.Join(filesDir, "current.yml")
	require.NoError(t, os.WriteFile(managedPath, []byte("current: true\n"), 0o600))

	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), t.TempDir(), dataDir)
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		managedPath: {Hash: "sha256:abc", Category: "file"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesRemoved)
	assert.Equal(t, 0, result.SecretsRemoved)
}

func TestScanMarkedDir_RemovesOrphans(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	// Write an orphan with the picolet marker
	orphanPath := filepath.Join(systemdDir, "old.service")
	content := config.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(orphanPath, []byte(content), 0o600))

	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(orphanPath).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), systemdDir, t.TempDir())
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

	fw := appliermocks.NewMockFileWriter(t)
	// No Remove calls expected
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), systemdDir, t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
}

func TestScanMarkedDir_KeepsManagedFiles(t *testing.T) {
	t.Parallel()
	systemdDir := t.TempDir()

	managedPath := filepath.Join(systemdDir, "managed.service")
	content := config.PicoletMarker + "\n[Service]\nExecStart=/bin/true\n"
	require.NoError(t, os.WriteFile(managedPath, []byte(content), 0o600))

	fw := appliermocks.NewMockFileWriter(t)
	// No Remove calls expected — file is in managedFiles
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), systemdDir, t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		managedPath: {Hash: "sha256:abc", Category: "systemd"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.FilesRemoved)
}

func TestScanSecrets_RemovesOrphans(t *testing.T) {
	t.Parallel()
	fw := appliermocks.NewMockFileWriter(t)
	pod := appliermocks.NewMockPodmanClient(t)
	// "kept" is in state, "orphan" is not
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"kept", "orphan"}, nil)
	pod.EXPECT().SecretRemove(mock.Anything, "orphan").Return(nil)

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{
		"secret:kept": {Hash: "sha256:abc", Category: "secret"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.SecretsRemoved)
}

func TestScanSecrets_KeepsAllManaged(t *testing.T) {
	t.Parallel()
	fw := appliermocks.NewMockFileWriter(t)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"db-pass", "api-key"}, nil)
	// No SecretRemove calls expected

	s := orphan.New(fw, pod, appliermocks.NewMockSystemdManager(t), t.TempDir(), t.TempDir(), t.TempDir())
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

	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(mock.Anything).Return(nil).Times(2)

	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return([]string{"orphan-secret"}, nil)
	pod.EXPECT().SecretRemove(mock.Anything, "orphan-secret").Return(nil)

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().StopUnit(mock.Anything, "a.service").Return(nil)
	sys.EXPECT().StopUnit(mock.Anything, "b.service").Return(nil)
	s := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir())
	result, err := s.Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.FilesRemoved)
	assert.Equal(t, 1, result.SecretsRemoved)
}

// A stale Quadlet's generated service keeps running after daemon-reload drops
// its definition, so it is stopped before the file is removed.
func TestScan_StaleQuadletStopsItsGeneratedService(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		file     string
		content  string
		wantStop string // "" = no stop expected
		stopErr  error
	}{
		{name: "pod", file: "web.pod", content: "[Pod]\nPublishPort=8080:80\n", wantStop: "web-pod.service"},
		{name: "ServiceName= override", file: "web.pod", content: "[Pod]\nServiceName=shop\n", wantStop: "shop.service"},
		{name: "network", file: "lan.network", content: "[Network]\n", wantStop: "lan-network.service"},
		{name: "failed stop still removes the file", file: "web.pod", content: "[Pod]\n", wantStop: "web-pod.service", stopErr: errors.New("unit web-pod.service not loaded")},
		{name: "agent's own user unit is not stopped", file: "picolet.container", content: "[Container]\nImage=picolet\n"},
		{name: "agent's own system unit is not stopped", file: "picolet-system.container", content: "[Container]\nImage=picolet\n"},
		{name: "unparseable file is still removed", file: "broken.container", content: "[Container\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			quadletDir := t.TempDir()
			path := filepath.Join(quadletDir, tt.file)
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			sys := appliermocks.NewMockSystemdManager(t)
			fw := appliermocks.NewMockFileWriter(t)
			remove := fw.EXPECT().Remove(path).Return(nil).Once()
			if tt.wantStop != "" {
				remove.NotBefore(sys.EXPECT().StopUnit(mock.Anything, tt.wantStop).Return(tt.stopErr).Once())
			}
			pod := appliermocks.NewMockPodmanClient(t)
			pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

			result, err := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir()).
				Scan(context.Background(), map[string]state.ManagedFile{})
			require.NoError(t, err)
			assert.Equal(t, 1, result.FilesRemoved)
		})
	}
}

// Stopping a pod stops its members (BindsTo=). A stale pod the agent's own
// container joins must therefore not be stopped, whether that container is
// still managed or stale itself (e.g. after a state reset).
func TestScan_StalePodWithAgentMemberIsNotStopped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		agentFile    string
		agentContent string
		agentIsStale bool
	}{
		{name: "managed agent container", agentFile: "picolet.container", agentContent: "[Container]\nImage=picolet\nPod=web.pod\n"},
		{name: "stale agent container", agentFile: "picolet.container", agentContent: "[Container]\nImage=picolet\nPod=web.pod\n", agentIsStale: true},
		{name: "agent unit via ServiceName=", agentFile: "agent.container", agentContent: "[Container]\nImage=picolet\nServiceName=picolet-system\nPod=web.pod\n", agentIsStale: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			quadletDir := t.TempDir()
			podPath := filepath.Join(quadletDir, "web.pod")
			agentPath := filepath.Join(quadletDir, tt.agentFile)
			require.NoError(t, os.WriteFile(podPath, []byte("[Pod]\n"), 0o600))
			require.NoError(t, os.WriteFile(agentPath, []byte(tt.agentContent), 0o600))

			sys := appliermocks.NewMockSystemdManager(t) // no StopUnit expected
			fw := appliermocks.NewMockFileWriter(t)
			fw.EXPECT().Remove(podPath).Return(nil).Once()
			managed := map[string]state.ManagedFile{agentPath: {Hash: "sha256:abc", Category: "container"}}
			if tt.agentIsStale {
				fw.EXPECT().Remove(agentPath).Return(nil).Once()
				managed = map[string]state.ManagedFile{}
			}
			pod := appliermocks.NewMockPodmanClient(t)
			pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

			_, err := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir()).
				Scan(context.Background(), managed)
			require.NoError(t, err)
		})
	}
}

// writeFile creates path (and its parent directories) with content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// Whether a stale pod is stopped depends on the agent container's settings as
// Podman sees them: the unit merged with its drop-ins from every unit
// directory. If those cannot be read, no pod is stopped. Either way the pod
// file is removed and other stale units are still stopped.
//
//nolint:funlen // table of filesystem setups
func TestScan_StalePodStopFollowsAgentContainerSettings(t *testing.T) {
	t.Parallel()
	const agentUnit = "[Container]\nImage=picolet\n"
	tests := []struct {
		name string
		// setup writes into the Quadlet dir and a second Podman unit dir, and
		// returns the Quadlet-dir paths to keep managed (not orphans).
		setup      func(t *testing.T, quadletDir, otherUnitDir string) []string
		podStopped bool
	}{
		{name: "unparseable container", setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "agent.container"), "[Container\nPod=web.pod\n")
			return []string{filepath.Join(dir, "agent.container")}
		}},
		{name: "container vanished (dangling symlink)", setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			path := filepath.Join(dir, "agent.container")
			require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), path))
			return []string{path}
		}},
		{name: "drop-in next to the unit adds Pod=", setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(dir, "picolet.container.d", "10-pod.conf"), "[Container]\nPod=web.pod\n")
			return []string{filepath.Join(dir, "picolet.container"), filepath.Join(dir, "picolet.container.d", "10-pod.conf")}
		}},
		{name: "drop-in in another unit dir adds Pod=", setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(other, "picolet.container.d", "10-pod.conf"), "[Container]\nPod=web.pod\n")
			return []string{filepath.Join(dir, "picolet.container")}
		}},
		{name: "top-level container.d adds Pod= to every container", setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(other, "container.d", "10-pod.conf"), "[Container]\nPod=web.pod\n")
			return []string{filepath.Join(dir, "picolet.container")}
		}},
		{name: "drop-in makes another container the agent via ServiceName=", setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "agent.container"), "[Container]\nImage=picolet\nPod=web.pod\n")
			writeFile(t, filepath.Join(other, "agent.container.d", "10-name.conf"), "[Container]\nServiceName=picolet\n")
			return []string{filepath.Join(dir, "agent.container")}
		}},
		{name: "symlinked drop-in directory (Podman follows it)", setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			target := t.TempDir()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(target, "10-pod.conf"), "[Container]\nPod=web.pod\n")
			require.NoError(t, os.Symlink(target, filepath.Join(dir, "picolet.container.d")))
			return []string{filepath.Join(dir, "picolet.container"), filepath.Join(dir, "picolet.container.d")}
		}},
		{name: "unparseable drop-in", setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(other, "picolet.container.d", "10-pod.conf"), "[Container\n")
			return []string{filepath.Join(dir, "picolet.container")}
		}},
		{name: "empty leftover drop-in directory", podStopped: true, setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "picolet.container.d"), 0o700))
			return []string{filepath.Join(dir, "picolet.container")}
		}},
		{name: "plain file named like a drop-in directory", podStopped: true, setup: func(t *testing.T, dir, _ string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "junk.d"), "x")
			return []string{filepath.Join(dir, "junk.d")}
		}},
		{name: "drop-in for another container adds Pod=", podStopped: true, setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), agentUnit)
			writeFile(t, filepath.Join(other, "api.container.d", "10-pod.conf"), "[Container]\nPod=web.pod\n")
			return []string{filepath.Join(dir, "picolet.container")}
		}},
		{name: "agent container lives only in another unit dir", setup: func(t *testing.T, _, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(other, "picolet.container"), "[Container]\nImage=picolet\nPod=web.pod\n")
			return nil
		}},
		{name: "higher-priority unit dir shadows the Quadlet-dir container", podStopped: true, setup: func(t *testing.T, dir, other string) []string {
			t.Helper()
			writeFile(t, filepath.Join(dir, "picolet.container"), "[Container]\nImage=picolet\nPod=web.pod\n")
			writeFile(t, filepath.Join(other, "picolet.container"), agentUnit)
			return []string{filepath.Join(dir, "picolet.container")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			quadletDir, otherUnitDir := t.TempDir(), t.TempDir()
			podPath := filepath.Join(quadletDir, "web.pod")
			netPath := filepath.Join(quadletDir, "lan.network")
			writeFile(t, podPath, "[Pod]\n")
			writeFile(t, netPath, "[Network]\n")
			managed := map[string]state.ManagedFile{}
			for _, path := range tt.setup(t, quadletDir, otherUnitDir) {
				managed[path] = state.ManagedFile{Hash: "sha256:abc", Category: "container"}
			}

			sys := appliermocks.NewMockSystemdManager(t)
			sys.EXPECT().StopUnit(mock.Anything, "lan-network.service").Return(nil).Once()
			if tt.podStopped {
				sys.EXPECT().StopUnit(mock.Anything, "web-pod.service").Return(nil).Once()
			}
			fw := appliermocks.NewMockFileWriter(t)
			fw.EXPECT().Remove(podPath).Return(nil).Once()
			fw.EXPECT().Remove(netPath).Return(nil).Once()
			pod := appliermocks.NewMockPodmanClient(t)
			pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

			result, err := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir(), orphan.WithUnitDirs(otherUnitDir)).
				Scan(context.Background(), managed)
			require.NoError(t, err)
			assert.Equal(t, 2, result.FilesRemoved)
		})
	}
}

// A symlinked owned directory is not walked (WalkDir does not follow it), so
// the scan would otherwise take the link itself for an orphaned file and
// delete it. It reports an error and removes nothing instead.
func TestScan_SymlinkedOwnedDirIsNotRemoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// link returns the quadletDir and dataDir to scan, one of them symlinked.
		link func(t *testing.T, target string) (quadletDir, dataDir string)
	}{
		{name: "Quadlet directory", link: func(t *testing.T, target string) (string, string) {
			t.Helper()
			link := filepath.Join(t.TempDir(), "picolet")
			require.NoError(t, os.Symlink(target, link))
			return link, t.TempDir()
		}},
		{name: "data subdirectory", link: func(t *testing.T, target string) (string, string) {
			t.Helper()
			dataDir := t.TempDir()
			require.NoError(t, os.Symlink(target, filepath.Join(dataDir, "files")))
			return t.TempDir(), dataDir
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := t.TempDir()
			writeFile(t, filepath.Join(target, "web.pod"), "[Pod]\n")
			quadletDir, dataDir := tt.link(t, target)

			sys := appliermocks.NewMockSystemdManager(t) // nothing stopped
			fw := appliermocks.NewMockFileWriter(t)      // nothing removed
			pod := appliermocks.NewMockPodmanClient(t)

			result, err := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), dataDir).
				Scan(context.Background(), map[string]state.ManagedFile{})
			require.ErrorContains(t, err, "is a symlink")
			assert.Zero(t, result.FilesRemoved)
		})
	}
}

// The stopped service follows the stale unit's drop-ins too: a drop-in may
// rename it with ServiceName=.
func TestScan_StaleQuadletServiceNameFromDropIn(t *testing.T) {
	t.Parallel()
	quadletDir, otherUnitDir := t.TempDir(), t.TempDir()
	podPath := filepath.Join(quadletDir, "web.pod")
	writeFile(t, podPath, "[Pod]\n")
	writeFile(t, filepath.Join(otherUnitDir, "web.pod.d", "10-name.conf"), "[Pod]\nServiceName=shop\n")

	sys := appliermocks.NewMockSystemdManager(t)
	stop := sys.EXPECT().StopUnit(mock.Anything, "shop.service").Return(nil).Once()
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().Remove(podPath).Return(nil).Once().NotBefore(stop)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)

	_, err := orphan.New(fw, pod, sys, quadletDir, t.TempDir(), t.TempDir(), orphan.WithUnitDirs(otherUnitDir)).
		Scan(context.Background(), map[string]state.ManagedFile{})
	require.NoError(t, err)
}
