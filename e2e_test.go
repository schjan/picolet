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
		s = strings.TrimPrefix(s, "unix:")
		if _, err := os.Stat(s); err == nil { //nolint:gosec // env-controlled path in E2E test
			return s
		}
	}
	fallback := fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
	if _, err := os.Stat(fallback); err != nil {
		t.Fatal("podman socket not available: checked $PODMAN_SOCKET and", fallback)
	}
	return fallback
}

// ciBranch returns the git branch to clone in CI, falling back to "main".
func ciBranch() string {
	if ref := os.Getenv("GITHUB_HEAD_REF"); ref != "" {
		return ref
	}
	if ref := os.Getenv("GITHUB_REF_NAME"); ref != "" {
		return ref
	}
	return "main"
}

// writeTokenFile persists GITHUB_TOKEN to a temp file and returns its path, or "" if unset.
func writeTokenFile(t *testing.T) string {
	t.Helper()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return ""
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0o600)) //nolint:gosec // env-controlled path in E2E test
	return tokenFile
}

//nolint:funlen,tparallel // E2E test with intentionally sequential sub-tests exercising the full pipeline
func TestE2EPipeline(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	branch := ciBranch()
	repoURL := "https://github.com/schjan/picolet.git"
	tokenPath := writeTokenFile(t)

	cloneDir := filepath.Join(t.TempDir(), "repo")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")
	secretsDir := t.TempDir()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	quadletDir := filepath.Join(home, ".config", "containers", "systemd")
	systemdDir := filepath.Join(home, ".config", "systemd", "user")

	// Create clients in parent scope for cleanup and sub-test reuse
	podman, err := applier.NewSocketPodmanClient(t.Context(), socketPath)
	require.NoError(t, err)

	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)

	// Register cleanup on parent t so it runs after ALL sub-tests (including verify)
	t.Cleanup(func() {
		// Remove only files that picolet actually wrote, based on saved state
		st, loadErr := state.NewStore(statePath).Load()
		if loadErr == nil {
			for destPath := range st.ManagedFiles {
				if strings.HasPrefix(destPath, "secret:") {
					continue
				}
				_ = os.Remove(destPath)
			}
		}
		// Daemon-reload to clean up generated units and remove the container
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = systemd.DaemonReload(cleanupCtx)
		_ = podman.ContainerRemove(cleanupCtx, "picolet-e2e-test", true)
		systemd.Close()
	})

	// Shared state between sequential sub-tests
	var headSHA string

	t.Run("clone", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
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

	// Agent and store are shared between reconcile and idempotent sub-tests
	metrics.Register()

	agentCfg := &agentcfg.Config{
		Hostname:     "e2e-host",
		RepoURL:      repoURL,
		RepoBranch:   branch,
		PollInterval: time.Minute,
		MetricsPort:  0,
		SecretsDir:   secretsDir,
		Rootless:     true,
	}

	a := agent.New(agentCfg,
		agent.WithRepoPath(filepath.Join(cloneDir, "testdata", "example-fleet")),
		agent.WithFileWriter(applier.NewAtomicFileWriter()),
		agent.WithPodman(podman),
		agent.WithSystemd(systemd),
		agent.WithLockPath(lockPath),
		agent.WithStatePath(statePath),
	)
	store := state.NewStore(statePath)

	t.Run("reconcile", func(t *testing.T) {
		require.NotEmpty(t, headSHA, "clone sub-test must have set headSHA")

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		emptyState := &state.State{ManagedFiles: make(map[string]string)}
		result, err := a.ReconcileOnce(ctx, headSHA, emptyState, store)
		require.NoError(t, err, "ReconcileOnce should succeed")
		assert.True(t, result.HasChanges, "first reconcile should have changes")
		require.NotNil(t, result.ApplyResult, "apply result should be present")
		assert.Empty(t, result.ApplyResult.Errors, "apply should have no errors")
	})

	t.Run("idempotent", func(t *testing.T) {
		require.NotEmpty(t, headSHA, "clone sub-test must have set headSHA")

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		st, err := store.Load()
		require.NoError(t, err)

		result, err := a.ReconcileOnce(ctx, headSHA, st, store)
		require.NoError(t, err, "idempotent ReconcileOnce should succeed")
		assert.False(t, result.HasChanges, "second reconcile with same state should be a no-op")
		assert.Nil(t, result.ApplyResult, "no apply should have run")
	})

	t.Run("verify", func(t *testing.T) {
		t.Run("quadlet_files_written", func(t *testing.T) {
			containerFile := filepath.Join(quadletDir, "simple.container")
			data, err := os.ReadFile(containerFile)
			require.NoError(t, err, "simple.container should exist at %s", containerFile)

			content := string(data)
			assert.Contains(t, content, "[Container]")
			assert.Contains(t, content, "alpine:3.21", "image should be rendered from template")
			assert.Contains(t, content, "Network=internal.network", "container should reference internal network")
			assert.Contains(t, content, "Secret=e2e_secret", "container should mount the secret")
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
				_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
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
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			connCtx, err := bindings.NewConnection(ctx, "unix:"+socketPath)
			require.NoError(t, err)

			inspectData, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
			require.NoError(t, err, "container picolet-e2e-test should exist")

			assert.Equal(t, "running", inspectData.State.Status, "container should be running")
			assert.Contains(t, inspectData.ImageName, "alpine:3.21", "container image should be alpine:3.21")

			// Verify labels were rendered correctly from template
			labels := inspectData.Config.Labels
			assert.Equal(t, "e2e-host", labels["hostname"], "hostname label should match")
			assert.Equal(t, "e2e-host.test", labels["external"], "external hostname label should match")
		})

		t.Run("secret_content_in_container", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			out, err := exec.CommandContext(ctx, "podman", "exec", "picolet-e2e-test", "cat", "/run/secrets/e2e_secret").Output()
			require.NoError(t, err, "should be able to read secret inside container")
			assert.Equal(t, "e2e-test-secret-data\n", string(out), "secret content should match")
		})

		t.Run("secret_in_podman", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

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
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

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

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg, Rootless: true})
	require.NoError(t, err)
	resolved, err := r.ResolveHost("e2e-host")
	require.NoError(t, err)

	destPaths := make(map[string]bool, len(resolved.Files))
	for _, f := range resolved.Files {
		destPaths[f.DestPath] = true
	}

	quadletDir := filepath.Join(home, ".config", "containers", "systemd")
	systemdDir := filepath.Join(home, ".config", "systemd", "user")

	assert.True(t, destPaths[filepath.Join(quadletDir, "simple.container")],
		"simple.container should be in rootless quadlet dir")
	assert.True(t, destPaths[filepath.Join(quadletDir, "internal.network")],
		"internal.network should be in rootless quadlet dir")
	assert.True(t, destPaths[filepath.Join(systemdDir, "custom.socket")],
		"custom.socket should be in rootless systemd dir")

	for path := range destPaths {
		if strings.HasPrefix(path, "secret:") {
			continue
		}
		assert.NotContains(t, path, "/etc/", "rootless paths should not use /etc/")
		assert.NotContains(t, path, "/var/lib/", "rootless paths should not use /var/lib/")
	}
}
