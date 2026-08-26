//go:build e2e

package e2e_test

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
	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/orphan"
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

// ciBranch returns the git branch to clone.
// In CI it uses GitHub env vars; locally it reads the current branch from git.
func ciBranch() string {
	if b := cmp.Or(os.Getenv("GITHUB_HEAD_REF"), os.Getenv("GITHUB_REF_NAME")); b != "" {
		return b
	}
	if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		if b := strings.TrimSpace(string(out)); b != "" && b != "HEAD" {
			return b
		}
	}
	return "main"
}

// repoAuth returns the repo URL and token file path based on the environment.
// In CI (GITHUB_TOKEN set), it uses HTTPS with token auth.
// Locally (no token), it uses SSH so the user's SSH agent handles authentication.
func repoAuth(t *testing.T) (repoURL, tokenPath string) {
	t.Helper()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return "git@github.com:schjan/picolet.git", ""
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0o600)) //nolint:gosec // env-controlled path in E2E test
	return "https://github.com/schjan/picolet.git", tokenFile
}

//nolint:funlen,tparallel // E2E test with intentionally sequential sub-tests exercising the full pipeline
func TestE2EPipeline(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	branch := ciBranch()
	repoURL, tokenPath := repoAuth(t)

	cloneDir := filepath.Join(t.TempDir(), "repo")
	fleetDir := filepath.Join(cloneDir, "testdata", "example-fleet")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")
	secretsDir := t.TempDir()

	// Write the real secret value to the local secrets dir (secrets are never read from git)
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "e2e_secret.txt"), []byte("e2e-test-secret-data\n"), 0o600))

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	quadletDir := filepath.Join(home, ".config", "containers", "systemd", "picolet")
	systemdDir := filepath.Join(home, ".config", "systemd", "user")

	// Create clients in parent scope for cleanup and sub-test reuse.
	// Use context.Background() because Podman bindings embed the connection in the
	// context — t.Context() is cancelled before t.Cleanup runs, which would cause
	// cleanup calls (SecretRemove, ContainerRemove) to silently fail.
	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)

	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)

	// Pre-cleanup: remove stale resources from a previous interrupted run.
	// Without this, reconcile fails with "secret name in use" if e2e_secret persists.
	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = podman.SecretRemove(preCtx, "e2e_secret")
	_ = podman.ContainerRemove(preCtx, "picolet-e2e-test", true)
	_ = podman.ContainerRemove(preCtx, "systemd-extra", true)
	preCancel()

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
		// Disable any enabled raw systemd units so their *.target.wants symlinks
		// never leak (the file removal above does not remove enable symlinks).
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, unit := range []string{"maintenance.timer", "custom.socket", "failing.timer"} {
			_ = systemd.StopUnit(cleanupCtx, unit)
			_ = systemd.DisableUnit(cleanupCtx, unit)
		}
		// Daemon-reload to clean up generated units and remove the container
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

		poller := gitpoll.New(repoURL, branch, cloneDir, gitpoll.NewTokenFileAuth(tokenPath))
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
	metrics.Register(nil)

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
		agent.WithRepoPath(fleetDir),
		agent.WithFileWriter(applier.NewAtomicFileWriter()),
		agent.WithPodman(podman),
		agent.WithSystemd(systemd),
		agent.WithLockPath(lockPath),
		agent.WithStatePath(statePath),
	)
	store := state.NewStore(statePath)
	healthChecker := health.New(systemd)

	t.Run("reconcile", func(t *testing.T) {
		require.NotEmpty(t, headSHA, "clone sub-test must have set headSHA")

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		emptyState := state.NewState()
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

	// The committed base.systemd deploys maintenance.timer (with [Install]) and a
	// oneshot maintenance.service (no [Install]). The units stay deployed through
	// the sequential flow; the remove_all sub-test below asserts they are disabled
	// and removed on delete.
	t.Run("maintenance_timer", func(t *testing.T) {
		t.Run("timer_enabled_and_waiting", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			_, err := os.Stat(filepath.Join(systemdDir, "maintenance.timer"))
			require.NoError(t, err, "maintenance.timer should be written")

			status, err := systemd.GetUnitStatus(ctx, "maintenance.timer")
			require.NoError(t, err)
			assert.Equal(t, "enabled", status.UnitFileState, "timer should be enabled")
			assert.Equal(t, "active", status.ActiveState, "timer should be active")
			assert.Equal(t, "waiting", status.SubState, "timer should be waiting for next trigger")
		})

		t.Run("oneshot_present_but_not_enabled", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			_, err := os.Stat(filepath.Join(systemdDir, "maintenance.service"))
			require.NoError(t, err, "maintenance.service should be written")

			status, err := systemd.GetUnitStatus(ctx, "maintenance.service")
			require.NoError(t, err)
			assert.NotEqual(t, "enabled", status.UnitFileState,
				"oneshot with no [Install] must not be enabled (expected static/disabled)")
		})

		t.Run("manual_trigger_runs_oneshot", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			require.NoError(t, systemd.StartUnit(ctx, "maintenance.service"))
			require.Eventually(t, func() bool {
				status, err := systemd.GetUnitStatus(ctx, "maintenance.service")
				return err == nil && status.ActiveState == "inactive" && status.SubState == "dead"
			}, 10*time.Second, 250*time.Millisecond, "oneshot /bin/true should complete and go inactive")
		})
	})

	t.Run("orphan_scan", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		st, err := store.Load()
		require.NoError(t, err)

		dataDir := filepath.Join(home, ".local", "share", "picolet")

		// 1. Plant orphan quadlet file (picolet-owned dir — all files safe to remove)
		orphanQuadlet := filepath.Join(quadletDir, "orphan.container")
		require.NoError(t, os.WriteFile(orphanQuadlet, []byte("[Container]\nImage=ghost\n"), 0o644))

		// 2. Plant orphan systemd file WITH picolet marker (shared dir — marker required)
		orphanSystemd := filepath.Join(systemdDir, "orphan-picolet.service")
		content := config.PicoletMarker + "\n[Unit]\nDescription=orphan\n"
		require.NoError(t, os.WriteFile(orphanSystemd, []byte(content), 0o644))

		// 3. Plant foreign systemd file WITHOUT marker — must NOT be removed
		foreignSystemd := filepath.Join(systemdDir, "foreign.service")
		require.NoError(t, os.WriteFile(foreignSystemd, []byte("[Unit]\nDescription=foreign\n"), 0o644))
		t.Cleanup(func() { _ = os.Remove(foreignSystemd) })

		// 4. Create orphan Podman secret (labeled managed-by=picolet by SecretCreate)
		require.NoError(t, podman.SecretCreate(ctx, "e2e_orphan_secret", []byte("orphan-data"), false))
		t.Cleanup(func() { _ = podman.SecretRemove(context.Background(), "e2e_orphan_secret") })

		// Check if Podman supports secret labels (older versions silently ignore them).
		// ListManagedSecrets relies on the managed-by=picolet label to identify orphans.
		managedSecrets, err := podman.ListManagedSecrets(ctx)
		require.NoError(t, err)
		secretLabelsSupported := len(managedSecrets) > 0

		// 5. Run the orphan scanner
		scanner := orphan.New(applier.NewAtomicFileWriter(), podman, quadletDir, systemdDir, dataDir)
		scanResult, err := scanner.Scan(ctx, st.ManagedFiles)
		require.NoError(t, err)
		assert.True(t, scanResult.FilesRemoved > 0 || scanResult.SecretsRemoved > 0, "scanner should report removals")

		// 6. Call DaemonReload (mirrors what agent.scanOrphans does after removals)
		require.NoError(t, systemd.DaemonReload(ctx))

		// 7. Assert orphans are gone
		assert.NoFileExists(t, orphanQuadlet, "orphan quadlet file should be removed")
		assert.NoFileExists(t, orphanSystemd, "orphan systemd file should be removed")

		// 8. Assert foreign file is untouched
		assert.FileExists(t, foreignSystemd, "foreign systemd file must not be removed")

		// 9. Assert orphan secret is gone (only when Podman supports secret labels)
		if secretLabelsSupported {
			exists, err := podman.SecretExists(ctx, "e2e_orphan_secret")
			require.NoError(t, err)
			assert.False(t, exists, "orphan Podman secret should be removed")
		} else {
			t.Log("skipping orphan secret assertion: Podman version does not support secret labels")
		}

		// 10. Assert managed files are still on disk
		assert.FileExists(t, filepath.Join(quadletDir, "simple.container"), "managed file must survive scan")
		assert.FileExists(t, filepath.Join(quadletDir, "internal.network"), "managed network must survive scan")
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

			simpleContainerKey := filepath.Join(home, ".config", "containers", "systemd", "picolet", "simple.container")
			assert.Contains(t, st.ManagedFiles, simpleContainerKey, "should contain simple.container path")

			assert.Empty(t, st.FailedSHA, "failed SHA should be empty")

			// ServiceNames must be populated for quadlets so the health checker can read them
			networkKey := filepath.Join(quadletDir, "internal.network")
			assert.Equal(t, "simple.service", st.ServiceNames[simpleContainerKey])
			assert.Equal(t, "internal-network.service", st.ServiceNames[networkKey])
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
		oldMF := st.ManagedFiles["secret:e2e_secret"]
		require.NotEmpty(t, oldMF.Hash, "secret should be in managed files")

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
			assert.NotEqual(t, oldMF.Hash, st.ManagedFiles["secret:e2e_secret"].Hash,
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

		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, "assignments.yml"),
			[]byte(e2eAssignments([]string{
				"quadlets/containers/simple.container.tmpl",
				"quadlets/containers/extra.container",
			}, true)), 0o644))

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

	t.Run("health_enforce_healthy", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		result, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		assert.Contains(t, result.Healthy, "simple.service")
		assert.Contains(t, result.Healthy, "extra.service")
		assert.Contains(t, result.Healthy, "internal-network.service")
		assert.Empty(t, result.Unhealthy, "all managed units should be active")
		assert.Empty(t, result.Errors, "no health check errors expected")
		// Note: custom.socket is not checked directly here — this asserts the quadlet
		// units; raw systemd units do carry a ServiceNames entry (resolver.go).
	})

	t.Run("health_enforce_restart", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()

		// Phase 1: force extra.service inactive — StopUnit is synchronous, no poll needed
		require.NoError(t, systemd.StopUnit(ctx, "extra.service"))

		// Phase 2: confirm detection — Enforce should see extra.service as unhealthy and call RestartUnit
		st, err := store.Load()
		require.NoError(t, err)
		result, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		assert.Contains(t, result.Unhealthy, "extra.service")
		assert.Len(t, result.Unhealthy, 1, "only extra.service should be unhealthy")
		assert.Empty(t, result.Errors)

		// Phase 3: confirm recovery — RestartUnit was called by Enforce; poll until active
		require.Eventually(t, func() bool {
			status, _ := systemd.GetUnitStatus(ctx, "extra.service")
			return status.ActiveState == "active"
		}, 60*time.Second, 2*time.Second,
			"extra.service should recover after health enforcement")

		result2, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		assert.Contains(t, result2.Healthy, "extra.service")
		assert.NotContains(t, result2.Unhealthy, "extra.service")
	})

	// A timer-triggered one-shot is systemd's to run, not picolet's: it is never
	// started/restarted on apply, never restarted by health-enforce, and the timer
	// re-invokes it on its own. Runs after health_enforce_restart; stage G (in
	// t.Cleanup) restores the assignments + AppliedSHA the next sub-test expects.
	//
	//nolint:funlen // sequential staged exercise of the full timer-oneshot contract
	t.Run("timer_triggered_oneshot", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
		defer cancel()

		e2eContainers := []string{
			"quadlets/containers/simple.container.tmpl",
			"quadlets/containers/extra.container",
		}
		servicePath := filepath.Join(fleetDir, "systemd", "failing.service")

		reconcile := func(t *testing.T, sha string, extraSystemd ...string) *agent.ReconcileResult {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(fleetDir, "assignments.yml"),
				[]byte(e2eAssignments(e2eContainers, true, extraSystemd...)), 0o644))
			st, err := store.Load()
			require.NoError(t, err)
			result, err := a.ReconcileOnce(ctx, sha, st, store)
			require.NoError(t, err)
			require.NotNil(t, result.ApplyResult)
			return result
		}

		// Stage G, registered first: remove both fixtures and restore AppliedSHA to
		// "update-sha" (what validation_failure asserts) even if a stage below fails.
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer ccancel()
			_ = os.WriteFile(filepath.Join(fleetDir, "assignments.yml"),
				[]byte(e2eAssignments(e2eContainers, true)), 0o644)
			if st, err := store.Load(); err == nil {
				_, _ = a.ReconcileOnce(cctx, "update-sha", st, store)
			}
		})

		// Raw D-Bus for the uint64 timestamp/trigger properties GetUnitStatus omits.
		conn, err := dbus.NewUserConnectionContext(ctx)
		require.NoError(t, err)
		defer conn.Close()

		activeEnter := func(t *testing.T, unit string) uint64 {
			t.Helper()
			prop, err := conn.GetUnitPropertyContext(ctx, unit, "ActiveEnterTimestamp")
			require.NoError(t, err)
			v, ok := prop.Value.Value().(uint64)
			require.True(t, ok, "ActiveEnterTimestamp should decode as uint64")
			return v
		}
		lastTrigger := func(t *testing.T, unit string) uint64 {
			t.Helper()
			props, err := conn.GetUnitTypePropertiesContext(ctx, unit, "Timer")
			require.NoError(t, err)
			v, ok := props["LastTriggerUSec"].(uint64)
			require.True(t, ok, "LastTriggerUSec should decode as uint64")
			return v
		}
		opsFor := func(ops []applier.SystemdUnitOp, unit string) []applier.SystemdUnitOp {
			var out []applier.SystemdUnitOp
			for _, o := range ops {
				if o.Unit == unit {
					out = append(out, o)
				}
			}
			return out
		}
		editService := func(t *testing.T, desc string) {
			t.Helper()
			require.NoError(t, os.WriteFile(servicePath, fmt.Appendf(nil,
				"[Unit]\nDescription=%s\nStartLimitIntervalSec=0\n\n[Service]\nType=oneshot\nExecStart=/bin/false\n",
				desc), 0o644))
		}
		requireFailed := func(t *testing.T, msg string) {
			t.Helper()
			require.Eventually(t, func() bool {
				status, err := systemd.GetUnitStatus(ctx, "failing.service")
				return err == nil && status.ActiveState == "failed"
			}, 30*time.Second, 1*time.Second, msg)
		}

		// A: deploy the service alone (no timer). A static one-shot is written but
		// never enabled/started, and must never have run.
		t.Run("A_service_only_not_run", func(t *testing.T) {
			result := reconcile(t, "failing-a-sha", "systemd/failing.service")
			assert.Empty(t, opsFor(result.ApplyResult.SystemdUnitOps, "failing.service"),
				"a no-[Install] one-shot must get no enable/start op")

			status, err := systemd.GetUnitStatus(ctx, "failing.service")
			require.NoError(t, err)
			assert.Equal(t, "static", status.UnitFileState)
			assert.Equal(t, "oneshot", status.ServiceType)
			assert.Empty(t, status.TriggeredBy)
			assert.Zero(t, activeEnter(t, "failing.service"), "service must never have run")
		})

		// B: an edit is applied but still never runs the job.
		t.Run("B_edit_not_run", func(t *testing.T) {
			editService(t, "Picolet e2e failing one-shot (edited)")
			result := reconcile(t, "failing-b-sha", "systemd/failing.service")
			assert.GreaterOrEqual(t, result.Summary[reconciler.ActionUpdate], 1)
			assert.Empty(t, opsFor(result.ApplyResult.SystemdUnitOps, "failing.service"))
			assert.Zero(t, activeEnter(t, "failing.service"), "an edit must not run the job")
		})

		// C: add the timer. It is enabled + started, and the trigger edge appears.
		t.Run("C_add_timer", func(t *testing.T) {
			result := reconcile(t, "failing-c-sha", "systemd/failing.service", "systemd/failing.timer")
			assert.ElementsMatch(t, []applier.SystemdUnitOp{
				{Unit: "failing.timer", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
				{Unit: "failing.timer", Operation: applier.SystemdOpStart, Result: applier.SystemdOpResultSuccess},
			}, opsFor(result.ApplyResult.SystemdUnitOps, "failing.timer"))

			status, err := systemd.GetUnitStatus(ctx, "failing.service")
			require.NoError(t, err)
			assert.Contains(t, status.TriggeredBy, "failing.timer",
				"the timer→service trigger edge should be installed (real 'as' decode)")
		})

		// D: the timer fires and the job fails.
		t.Run("D_job_fails", func(t *testing.T) {
			requireFailed(t, "the timer should fire and the one-shot should fail")
		})

		// E: neither health-enforce nor a content-changing reconcile restarts it.
		t.Run("E_never_restarted", func(t *testing.T) {
			for range 3 {
				st, err := store.Load()
				require.NoError(t, err)
				result, err := healthChecker.Enforce(ctx, st)
				require.NoError(t, err)
				assert.Contains(t, result.Unhealthy, "failing.service")
				assert.Contains(t, result.ExternallyActivated, "failing.service")
				assert.NotContains(t, result.Restarted, "failing.service")
				assert.NotContains(t, result.Skipped, "failing.service")
				status, err := systemd.GetUnitStatus(ctx, "failing.service")
				require.NoError(t, err)
				assert.Equal(t, "failed", status.ActiveState, "must stay failed, never restarted")
			}

			// A content-changing reconcile must not re-run the job either (defect 3).
			editService(t, "Picolet e2e failing one-shot (edited again)")
			result := reconcile(t, "failing-e-sha", "systemd/failing.service", "systemd/failing.timer")
			assert.Empty(t, opsFor(result.ApplyResult.SystemdUnitOps, "failing.service"))
			assert.NotContains(t, result.ApplyResult.RestartedUnits, "failing.service")
			requireFailed(t, "the job stays failed after the edit")
		})

		// F: the timer keeps re-invoking the job on its own.
		t.Run("F_timer_reinvokes", func(t *testing.T) {
			before := lastTrigger(t, "failing.timer")
			require.Eventually(t, func() bool {
				return lastTrigger(t, "failing.timer") > before
			}, 30*time.Second, 2*time.Second, "LastTriggerUSec should advance as the timer re-fires")
		})
	})

	t.Run("validation_failure", func(t *testing.T) {
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

		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(e2eAssignments([]string{
				"quadlets/containers/simple.container.tmpl",
				"quadlets/containers/extra.container",
				"quadlets/containers/bad.container",
			}, true)), 0o644))

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

		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(e2eAssignments([]string{
				"quadlets/containers/extra.container",
			}, true)), 0o644))

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

		t.Run("picolet_e2e_test_removed", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			// StopUnit triggers ExecStop=podman rm -v -f -i, which removes the container.
			// Poll until Inspect returns an error (container fully gone from Podman).
			require.Eventually(t, func() bool {
				_, inspectErr := containers.Inspect(connCtx, "picolet-e2e-test", nil)
				return inspectErr != nil // not found = fully removed
			}, 30*time.Second, 2*time.Second,
				"container picolet-e2e-test should be fully removed after simple.service is stopped")
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

			// Deleted container must not retain a ServiceNames entry
			assert.NotContains(t, st.ServiceNames, simpleKey, "removed container should be gone from ServiceNames")
			assert.Equal(t, "extra.service", st.ServiceNames[extraKey])
			assert.Equal(t, "internal-network.service", st.ServiceNames[networkKey])
		})
	})

	t.Run("remove_secret", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte(e2eAssignments([]string{
				"quadlets/containers/extra.container",
			}, false)), 0o644))

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

	t.Run("remove_all", func(t *testing.T) {
		st, err := store.Load()
		require.NoError(t, err)

		// Clear all assignments — every file in state becomes a delete
		require.NoError(t, os.WriteFile(
			filepath.Join(fleetDir, "assignments.yml"),
			[]byte("base: {}\nroles:\n  e2e: {}\n"), 0o644))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		result, err := a.ReconcileOnce(ctx, "remove-all-sha", st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		// extra.container + internal.network + custom.socket
		assert.GreaterOrEqual(t, result.Summary[reconciler.ActionDelete], 3)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)

		t.Run("network_removed", func(t *testing.T) {
			assert.NoFileExists(t, filepath.Join(quadletDir, "internal.network"))
		})

		t.Run("socket_removed", func(t *testing.T) {
			assert.NoFileExists(t, filepath.Join(systemdDir, "custom.socket"))
		})

		t.Run("maintenance_timer_disabled_and_removed", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			assert.NoFileExists(t, filepath.Join(systemdDir, "maintenance.timer"))
			assert.NoFileExists(t, filepath.Join(systemdDir, "maintenance.service"))

			// The delete path stops + disables the timer before removing the file;
			// after daemon-reload the unit is unknown or at least no longer enabled.
			if status, err := systemd.GetUnitStatus(ctx, "maintenance.timer"); err == nil {
				assert.NotEqual(t, "enabled", status.UnitFileState, "timer should be disabled after removal")
			}
		})

		t.Run("container_removed", func(t *testing.T) {
			assert.NoFileExists(t, filepath.Join(quadletDir, "extra.container"))
		})

		t.Run("extra_container_gone_from_podman", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				_, inspectErr := containers.Inspect(connCtx, "systemd-extra", nil)
				return inspectErr != nil
			}, 30*time.Second, 2*time.Second,
				"container systemd-extra should be fully removed after extra.service is stopped")
		})

		t.Run("state_empty", func(t *testing.T) {
			st, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "remove-all-sha", st.AppliedSHA)
			assert.Empty(t, st.ManagedFiles, "all managed files should be gone")
			assert.Empty(t, st.ServiceNames, "all service names should be gone")
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

// e2eAssignments builds an assignments.yml YAML string with the shared base
// structure, parameterising only the e2e role's containers and optional secret.
// extraSystemd appends additional base.systemd entries (e.g. the timer-triggered
// one-shot fixtures) without disturbing existing call sites.
func e2eAssignments(e2eContainers []string, withSecret bool, extraSystemd ...string) string {
	var sb strings.Builder
	sb.WriteString(`base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/custom.socket
    - systemd/custom.service
    - systemd/maintenance.timer
    - systemd/maintenance.service
`)
	for _, s := range extraSystemd {
		fmt.Fprintf(&sb, "    - %s\n", s)
	}
	sb.WriteString(`roles:
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
`)
	for _, c := range e2eContainers {
		fmt.Fprintf(&sb, "      - %s\n", c)
	}
	if withSecret {
		sb.WriteString("    secrets:\n      - secrets/e2e_secret.txt\n")
	}
	sb.WriteString(`features:
  app-a:
    containers:
      - quadlets/containers/nginx.container.tmpl
`)
	return sb.String()
}

// TestE2ESystemdPrivateSocket verifies that NewDBusSystemdManager connects to the user
// systemd instance via its private socket when rootless=true, and that a real method
// call succeeds over that connection.
func TestE2ESystemdPrivateSocket(t *testing.T) {
	t.Parallel()

	m, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err, "should connect to user systemd private socket")
	defer m.Close()

	err = m.DaemonReload(t.Context())
	require.NoError(t, err, "DaemonReload should succeed over the private socket connection")
}

// TestE2EResolverRootlessPaths verifies rootless path resolution produces correct paths.
func TestE2EResolverRootlessPaths(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	// `go test` sets CWD to this package dir (./e2e); testdata lives at the repo root.
	repoFS := os.DirFS("../testdata/example-fleet")
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg, Rootless: true})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "e2e-host")
	require.NoError(t, err)

	destPaths := make(map[string]bool, len(resolved.Files))
	for _, f := range resolved.Files {
		destPaths[f.DestPath] = true
	}

	quadletDir := filepath.Join(home, ".config", "containers", "systemd", "picolet")
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
