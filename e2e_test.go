//go:build e2e

package picolet_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

func podmanSocketPath(t *testing.T) string {
	t.Helper()
	if s := os.Getenv("PODMAN_SOCKET"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	fallback := fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
	if _, err := os.Stat(fallback); err != nil {
		t.Fatal("podman socket not available: checked $PODMAN_SOCKET and", fallback)
	}
	return fallback
}

func TestE2EPipeline(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)

	// Determine branch to clone
	branch := os.Getenv("GITHUB_HEAD_REF")
	if branch == "" {
		branch = os.Getenv("GITHUB_REF_NAME")
	}
	if branch == "" {
		branch = "main"
	}

	repoURL := "https://github.com/schjan/picolet.git"

	// Set up git token if available
	var tokenPath string
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		tokenFile := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0o600))
		tokenPath = tokenFile
	}

	cloneDir := filepath.Join(t.TempDir(), "repo")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")
	secretsDir := t.TempDir()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	quadletDir := filepath.Join(home, ".config", "containers", "systemd")
	systemdDir := filepath.Join(home, ".config", "systemd", "user")

	// Register cleanup on parent t so it runs after ALL sub-tests (including verify)
	t.Cleanup(func() {
		// Remove written quadlet files
		_ = filepath.Walk(quadletDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() {
				return nil
			}
			_ = os.Remove(path)
			return nil
		})
		// Remove written systemd units
		_ = filepath.Walk(systemdDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() {
				return nil
			}
			_ = os.Remove(path)
			return nil
		})
		// Daemon-reload to clean up generated units
		//nolint:gosec // test cleanup command
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		// Remove the container
		//nolint:gosec // test cleanup command
		_ = exec.Command("podman", "rm", "-f", "picolet-e2e-test").Run()
	})

	// Shared state between sequential sub-tests
	var headSHA string

	t.Run("clone", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		poller := gitpoll.New(repoURL, branch, cloneDir, tokenPath)
		require.NoError(t, poller.Init(ctx))

		result, err := poller.Poll(ctx, "")
		require.NoError(t, err)
		assert.Len(t, result.HeadSHA, 40, "HEAD SHA should be 40 chars")
		assert.True(t, result.Changed, "first poll should report changed")

		// Second poll with same SHA should report no change
		result2, err := poller.Poll(ctx, result.HeadSHA)
		require.NoError(t, err)
		assert.False(t, result2.Changed, "second poll with same SHA should not report changed")

		headSHA = result.HeadSHA
	})

	t.Run("reconcile", func(t *testing.T) {
		require.NotEmpty(t, headSHA, "clone sub-test must have set headSHA")

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		podman, err := applier.NewSocketPodmanClient(ctx, socketPath)
		require.NoError(t, err, "failed to connect to podman")

		systemd, err := applier.NewDBusSystemdManager(ctx, true)
		require.NoError(t, err, "failed to connect to session D-Bus")
		defer systemd.Close()

		metrics.Register()

		cfg := &agentcfg.Config{
			Hostname:     "e2e-host",
			RepoURL:      repoURL,
			RepoBranch:   branch,
			PollInterval: time.Minute,
			MetricsPort:  0,
			SecretsDir:   secretsDir,
			Rootless:     true,
		}

		a := agent.New(cfg,
			agent.WithRepoPath(filepath.Join(cloneDir, "testdata", "example-fleet")),
			agent.WithFileWriter(applier.NewAtomicFileWriter()),
			agent.WithPodman(podman),
			agent.WithSystemd(systemd),
			agent.WithLockPath(lockPath),
			agent.WithStatePath(statePath),
		)

		emptyState := &state.State{ManagedFiles: make(map[string]string)}
		store := state.NewStore(statePath)
		err = a.ReconcileOnce(ctx, headSHA, emptyState, store)
		require.NoError(t, err, "ReconcileOnce should succeed")
	})

	t.Run("verify", func(t *testing.T) {
		t.Run("quadlet_files_written", func(t *testing.T) {
			containerFile := filepath.Join(quadletDir, "simple.container")
			data, err := os.ReadFile(containerFile)
			require.NoError(t, err, "simple.container should exist at %s", containerFile)

			content := string(data)
			assert.Contains(t, content, "[Container]")
			assert.Contains(t, content, "alpine:3.21", "image should be rendered from template")
			assert.Contains(t, content, "hostname=e2e-host", "hostname label should be rendered")
			assert.Contains(t, content, "external=e2e-host.test", "external hostname label should be rendered")
			assert.NotContains(t, content, "{{", "template markers should be fully rendered")
		})

		t.Run("base_resources_written", func(t *testing.T) {
			networkFile := filepath.Join(quadletDir, "internal.network")
			data, err := os.ReadFile(networkFile)
			require.NoError(t, err, "internal.network should exist")
			assert.Contains(t, string(data), "[Network]")

			socketFile := filepath.Join(systemdDir, "custom.socket")
			_, err = os.ReadFile(socketFile)
			assert.NoError(t, err, "custom.socket should exist")
		})

		t.Run("no_template_markers", func(t *testing.T) {
			for _, dir := range []string{quadletDir, systemdDir} {
				_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return nil
					}
					content := string(data)
					assert.NotContains(t, content, "{{", "file %s should not contain template markers", path)
					assert.NotContains(t, content, "}}", "file %s should not contain template markers", path)
					return nil
				})
			}
		})

		t.Run("state_saved", func(t *testing.T) {
			store := state.NewStore(statePath)
			st, err := store.Load()
			require.NoError(t, err)

			assert.Equal(t, headSHA, st.AppliedSHA, "applied SHA should match")
			assert.NotEmpty(t, st.ManagedFiles, "managed files should be non-empty")
			assert.Contains(t, st.ManagedFiles, "secret:e2e_secret", "should contain e2e_secret")

			simpleContainerKey := filepath.Join(home, ".config", "containers", "systemd", "simple.container")
			assert.Contains(t, st.ManagedFiles, simpleContainerKey, "should contain simple.container path")

			assert.Empty(t, st.FailedSHA, "failed SHA should be empty")
		})

		t.Run("container_running", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			connCtx, err := bindings.NewConnection(ctx, "unix:"+socketPath)
			require.NoError(t, err)

			inspectData, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
			require.NoError(t, err, "container picolet-e2e-test should exist")

			assert.Equal(t, "running", inspectData.State.Status, "container should be running")
			assert.Contains(t, inspectData.ImageName, "alpine:3.21", "container image should be alpine:3.21")
		})

		t.Run("secret_in_podman", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			podman, err := applier.NewSocketPodmanClient(ctx, socketPath)
			require.NoError(t, err)

			t.Cleanup(func() {
				_ = podman.SecretRemove(context.Background(), "e2e_secret")
			})

			exists, err := podman.SecretExists(ctx, "e2e_secret")
			require.NoError(t, err)
			assert.True(t, exists, "e2e_secret should exist in podman")
		})
	})

	t.Run("podman_api", func(t *testing.T) {
		t.Run("secret_lifecycle", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			podman, err := applier.NewSocketPodmanClient(ctx, socketPath)
			require.NoError(t, err)

			secretName := fmt.Sprintf("picolet-e2e-%d", time.Now().UnixNano())
			t.Cleanup(func() {
				_ = podman.SecretRemove(context.Background(), secretName)
			})

			// 1. Secret should not exist initially
			exists, err := podman.SecretExists(ctx, secretName)
			require.NoError(t, err)
			assert.False(t, exists)

			// 2. Create secret
			err = podman.SecretCreate(ctx, secretName, []byte("test-data"), false)
			require.NoError(t, err)

			// 3. Should exist now
			exists, err = podman.SecretExists(ctx, secretName)
			require.NoError(t, err)
			assert.True(t, exists)

			// 4. Replace secret
			err = podman.SecretCreate(ctx, secretName, []byte("new-data"), true)
			require.NoError(t, err)

			// 5. Still exists
			exists, err = podman.SecretExists(ctx, secretName)
			require.NoError(t, err)
			assert.True(t, exists)

			// 6. Remove
			err = podman.SecretRemove(ctx, secretName)
			require.NoError(t, err)

			// 7. Gone
			exists, err = podman.SecretExists(ctx, secretName)
			require.NoError(t, err)
			assert.False(t, exists)
		})
	})
}

// TestE2EResolverRootlessPaths verifies rootless path resolution produces correct paths.
func TestE2EResolverRootlessPaths(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	repoFS := os.DirFS("testdata/example-fleet")
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r := resolver.New(repoFS, cfg, nil, true)
	resolved, err := r.ResolveHost("e2e-host")
	require.NoError(t, err)

	destPaths := make(map[string]bool)
	for _, f := range resolved.Files {
		destPaths[f.DestPath] = true
	}

	expectedQuadletDir := home + "/.config/containers/systemd/"
	expectedSystemdDir := home + "/.config/systemd/user/"

	// Check container file goes to quadlet dir
	assert.True(t, destPaths[expectedQuadletDir+"simple.container"],
		"simple.container should be in rootless quadlet dir")

	// Check base resources
	assert.True(t, destPaths[expectedQuadletDir+"internal.network"],
		"internal.network should be in rootless quadlet dir")
	assert.True(t, destPaths[expectedSystemdDir+"custom.socket"],
		"custom.socket should be in rootless systemd dir")

	// Verify no rootful paths
	for path := range destPaths {
		if strings.HasPrefix(path, "secret:") {
			continue
		}
		assert.NotContains(t, path, "/etc/", "rootless paths should not use /etc/")
		assert.NotContains(t, path, "/var/lib/", "rootless paths should not use /var/lib/")
	}
}
