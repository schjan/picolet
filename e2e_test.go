//go:build e2e

package picolet_test

import (
	"cmp"
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
	"github.com/schjan/picolet/pkg/reconciler"
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
	return cmp.Or(os.Getenv("GITHUB_HEAD_REF"), os.Getenv("GITHUB_REF_NAME"), "main")
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

	// Write the real secret value to the local secrets dir (secrets are never read from git)
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "e2e_secret.txt"), []byte("e2e-test-secret-data\n"), 0o600))

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
		_ = podman.ContainerRemove(cleanupCtx, "systemd-extra", true)
		_ = podman.SecretRemove(cleanupCtx, "e2e_secret")
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
			assert.Contains(t, content, "alpine:3.23", "image should be rendered from template")
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
				err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return nil //nolint:nilerr // skip unreadable entries gracefully
					}
					data, readErr := os.ReadFile(path)
					require.NoError(t, readErr, "should be able to read %s", path)
					content := string(data)
					assert.NotContains(t, content, "{{", "file %s should not contain template markers", path)
					assert.NotContains(t, content, "}}", "file %s should not contain template markers", path)
					return nil
				})
				assert.NoError(t, err, "walking %s should not fail", dir)
			}
		})

		t.Run("state_saved", func(t *testing.T) {
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
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)

			// Poll until container reaches running state (image pull + systemd start are async)
			require.Eventually(t, func() bool {
				data, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
				return err == nil && data.State.Status == "running"
			}, 60*time.Second, 2*time.Second, "container picolet-e2e-test should reach running state")

			// Validate config once we know it's running
			inspectData, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
			require.NoError(t, err)
			assert.Contains(t, inspectData.ImageName, "alpine:3.23", "container image should be alpine:3.23")

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

			exists, err := podman.SecretExists(ctx, "e2e_secret")
			require.NoError(t, err)
			assert.True(t, exists, "e2e_secret should exist in podman")
		})
	})

	t.Run("update_secret", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)
		oldHash := st.ManagedFiles["secret:e2e_secret"]
		require.NotEmpty(t, oldHash, "secret should be in managed files")

		// Secrets are read from secretsDir, NOT from git — change the local file
		require.NoError(t, os.WriteFile(
			filepath.Join(secretsDir, "e2e_secret.txt"),
			[]byte("updated-secret-data\n"), 0o600))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "update-secret-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionUpdate], 1)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors, "secret update should produce no errors")

		t.Run("state_hash_changed", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.NotEqual(t, oldHash, st.ManagedFiles["secret:e2e_secret"],
				"secret hash should be updated after content change")
			assert.Equal(t, "update-secret-sha", st.AppliedSHA)
		})

		t.Run("container_still_running", func(t *testing.T) {
			// Secret updates do NOT restart containers (applier skips changedUnits for secrets)
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			data, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
			require.NoError(t, err)
			assert.Equal(t, "running", data.State.Status)
		})
	})

	t.Run("add_container", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		fleetDir := filepath.Join(cloneDir, "testdata", "example-fleet")
		newAssignments := `base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/custom.socket
pi_types:
  controller:
    containers:
      - quadlets/containers/exporter.container
    volumes:
      - quadlets/volumes/data.volume
    kube:
      - quadlets/kube/app-stack.kube.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
  worker:
    containers:
      - quadlets/containers/exporter.container
  e2e:
    containers:
      - quadlets/containers/simple.container.tmpl
      - quadlets/containers/extra.container
    secrets:
      - secrets/e2e_secret.txt
features:
  app-a:
    containers:
      - quadlets/containers/nginx.container.tmpl
`
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, "assignments.yml"),
			[]byte(newAssignments), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "add-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionCreate], 1)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)

		t.Run("extra_file_written", func(t *testing.T) {
			_, err := os.Stat(filepath.Join(quadletDir, "extra.container"))
			assert.NoError(t, err, "extra.container should exist")
		})

		t.Run("simple_container_intact", func(t *testing.T) {
			_, err := os.Stat(filepath.Join(quadletDir, "simple.container"))
			assert.NoError(t, err, "simple.container should still exist")
		})

		t.Run("state_has_both", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			extraKey := filepath.Join(quadletDir, "extra.container")
			simpleKey := filepath.Join(quadletDir, "simple.container")
			assert.Contains(t, st.ManagedFiles, extraKey)
			assert.Contains(t, st.ManagedFiles, simpleKey)
		})
	})

	t.Run("update_container", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		// Confirm baseline: container is still running with the original image
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		baseline, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
		require.NoError(t, err)
		require.Equal(t, "running", baseline.State.Status)
		require.Contains(t, baseline.ImageName, "alpine:3.23",
			"container should be running with alpine:3.23 before the update")

		fleetPath := filepath.Join(cloneDir, "testdata", "example-fleet", "fleet.yml")
		fleetData, err := os.ReadFile(fleetPath)
		require.NoError(t, err)
		require.Contains(t, string(fleetData), "alpine:3.23",
			"baseline alpine version should be 3.23")
		updated := strings.ReplaceAll(string(fleetData), "alpine:3.23", "alpine:3.22")
		require.NoError(t, os.WriteFile(fleetPath, []byte(updated), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "update-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionUpdate], 1)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)

		t.Run("quadlet_file_updated", func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(quadletDir, "simple.container"))
			require.NoError(t, err)
			assert.Contains(t, string(data), "alpine:3.22")
		})

		t.Run("container_restarted", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				data, err := containers.Inspect(connCtx, "picolet-e2e-test", nil)
				return err == nil && data.State.Status == "running" &&
					strings.Contains(data.ImageName, "alpine:3.22")
			}, 60*time.Second, 2*time.Second,
				"container should be running with alpine:3.22")
		})

		t.Run("state_updated", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "update-sha", st.AppliedSHA)
			simpleKey := filepath.Join(quadletDir, "simple.container")
			assert.Contains(t, st.ManagedFiles, simpleKey)
			assert.Contains(t, st.ManagedFiles, "secret:e2e_secret")
		})
	})

	t.Run("validation_failure", func(t *testing.T) {
		fleetDir := filepath.Join(cloneDir, "testdata", "example-fleet")

		origAssignments, err := os.ReadFile(filepath.Join(fleetDir, "assignments.yml"))
		require.NoError(t, err)

		badPath := filepath.Join(fleetDir, "quadlets", "containers", "bad.container")

		// Register cleanup BEFORE making changes — runs even if test fails early
		t.Cleanup(func() {
			_ = os.WriteFile(filepath.Join(fleetDir, "assignments.yml"), origAssignments, 0o644)
			_ = os.Remove(badPath)
		})

		badContainer := `[Container]
Image=alpine:latest
Network=nonexistent.network

[Install]
WantedBy=default.target
`
		require.NoError(t, os.WriteFile(badPath, []byte(badContainer), 0o644))

		badAssignments := `base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/custom.socket
pi_types:
  controller:
    containers:
      - quadlets/containers/exporter.container
    volumes:
      - quadlets/volumes/data.volume
    kube:
      - quadlets/kube/app-stack.kube.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
  worker:
    containers:
      - quadlets/containers/exporter.container
  e2e:
    containers:
      - quadlets/containers/simple.container.tmpl
      - quadlets/containers/extra.container
      - quadlets/containers/bad.container
    secrets:
      - secrets/e2e_secret.txt
features:
  app-a:
    containers:
      - quadlets/containers/nginx.container.tmpl
`
		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(badAssignments), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		st, err := store.Load()
		require.NoError(t, err)
		_, err = a.ReconcileOnce(ctx, "bad-sha", st, store)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "validation")

		t.Run("state_unchanged", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "update-sha", st.AppliedSHA)
		})

		t.Run("bad_file_not_written", func(t *testing.T) {
			_, err := os.Stat(filepath.Join(quadletDir, "bad.container"))
			assert.True(t, os.IsNotExist(err), "bad.container should NOT be on disk")
		})
	})

	t.Run("remove_container", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		fleetDir := filepath.Join(cloneDir, "testdata", "example-fleet")
		removeAssignments := `base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/custom.socket
pi_types:
  controller:
    containers:
      - quadlets/containers/exporter.container
    volumes:
      - quadlets/volumes/data.volume
    kube:
      - quadlets/kube/app-stack.kube.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
  worker:
    containers:
      - quadlets/containers/exporter.container
  e2e:
    containers:
      - quadlets/containers/extra.container
    secrets:
      - secrets/e2e_secret.txt
features:
  app-a:
    containers:
      - quadlets/containers/nginx.container.tmpl
`
		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(removeAssignments), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "remove-container-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionDelete], 1)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)

		t.Run("simple_container_removed", func(t *testing.T) {
			assert.NoFileExists(t, filepath.Join(quadletDir, "simple.container"))
		})

		t.Run("picolet_e2e_test_stopped", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			// After DaemonReload removes simple.service, systemd stops the container.
			// This takes a few seconds, so poll until it's gone or no longer running.
			require.Eventually(t, func() bool {
				data, inspectErr := containers.Inspect(connCtx, "picolet-e2e-test", nil)
				if inspectErr != nil {
					return true // not found = container gone
				}
				return data.State.Status != "running"
			}, 30*time.Second, 2*time.Second,
				"container picolet-e2e-test should stop after simple.service is removed by daemon-reload")

			// Force-remove as cleanup so subsequent tests start clean
			_ = podman.ContainerRemove(ctx, "picolet-e2e-test", true)
		})

		t.Run("extra_still_exists", func(t *testing.T) {
			_, err := os.Stat(filepath.Join(quadletDir, "extra.container"))
			assert.NoError(t, err, "extra.container should still exist")
		})

		t.Run("state_cleaned", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "remove-container-sha", st.AppliedSHA)
			simpleKey := filepath.Join(quadletDir, "simple.container")
			extraKey := filepath.Join(quadletDir, "extra.container")
			networkKey := filepath.Join(quadletDir, "internal.network")
			assert.NotContains(t, st.ManagedFiles, simpleKey)
			assert.Contains(t, st.ManagedFiles, extraKey)
			assert.Contains(t, st.ManagedFiles, networkKey)
		})
	})

	t.Run("remove_secret", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		fleetDir := filepath.Join(cloneDir, "testdata", "example-fleet")
		noSecretAssignments := `base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/custom.socket
pi_types:
  controller:
    containers:
      - quadlets/containers/exporter.container
    volumes:
      - quadlets/volumes/data.volume
    kube:
      - quadlets/kube/app-stack.kube.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
  worker:
    containers:
      - quadlets/containers/exporter.container
  e2e:
    containers:
      - quadlets/containers/extra.container
features:
  app-a:
    containers:
      - quadlets/containers/nginx.container.tmpl
`
		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(noSecretAssignments), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "remove-secret-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionDelete], 1)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors,
			"secret removal should produce no errors (no unit restart)")

		t.Run("secret_gone", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			exists, err := podman.SecretExists(ctx, "e2e_secret")
			require.NoError(t, err)
			assert.False(t, exists, "e2e_secret should be removed from podman")
		})

		t.Run("state_no_secret", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "remove-secret-sha", st.AppliedSHA)
			assert.NotContains(t, st.ManagedFiles, "secret:e2e_secret")
			extraKey := filepath.Join(quadletDir, "extra.container")
			assert.Contains(t, st.ManagedFiles, extraKey)
		})

		t.Run("extra_intact", func(t *testing.T) {
			_, err := os.Stat(filepath.Join(quadletDir, "extra.container"))
			assert.NoError(t, err)
			networkFile := filepath.Join(quadletDir, "internal.network")
			_, err = os.Stat(networkFile)
			assert.NoError(t, err)
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

	assert.Contains(t, destPaths, filepath.Join(quadletDir, "simple.container"),
		"simple.container should be in rootless quadlet dir")
	assert.Contains(t, destPaths, filepath.Join(quadletDir, "internal.network"),
		"internal.network should be in rootless quadlet dir")
	assert.Contains(t, destPaths, filepath.Join(systemdDir, "custom.socket"),
		"custom.socket should be in rootless systemd dir")

	for _, f := range resolved.Files {
		if strings.HasPrefix(f.DestPath, "secret:") {
			continue
		}
		assert.NotContains(t, f.DestPath, "/etc/", "rootless paths should not use /etc/")
		assert.NotContains(t, f.DestPath, "/var/lib/", "rootless paths should not use /var/lib/")
	}
}
