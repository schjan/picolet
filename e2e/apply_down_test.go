//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/cli"
	"github.com/schjan/picolet/pkg/state"
)

// setupApplyDownFleet creates a minimal fleet repo in fleetDir with unique resource names
// that don't conflict with TestE2EPipeline's resources (different container name, secret,
// and quadlet filename — no shared base resources).
func setupApplyDownFleet(t *testing.T, fleetDir string) {
	t.Helper()

	dirs := []string{
		filepath.Join(fleetDir, "hosts", "apply-host"),
		filepath.Join(fleetDir, "quadlets", "containers"),
		filepath.Join(fleetDir, "secrets"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := map[string]string{
		"fleet.yml": `images:
  apply: "docker.io/library/alpine:3.23"
`,
		"assignments.yml": `base: {}
pi_types:
  apply:
    containers:
      - quadlets/containers/apply.container.tmpl
    secrets:
      - secrets/apply_secret.txt
`,
		filepath.Join("hosts", "apply-host", "host.yml"): `hostname: apply-host
external_hostname: apply-host.local
pi_type: apply
features: []
`,
		filepath.Join("quadlets", "containers", "apply.container.tmpl"): `[Container]
Image={{ index .Images "apply" }}
ContainerName=picolet-apply-test
Secret=apply_secret
Exec=sleep infinity

[Install]
WantedBy=default.target
`,
		filepath.Join("secrets", "apply_secret.txt"): "apply-test-secret-data\n",
	}

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, name), []byte(content), 0o644))
	}
}

// runCLI invokes the picolet CLI in-process via cli.Execute. The args slice must
// start with the subcommand (no leading "picolet"; runCLI adds it).
//
// Note: cli.Execute's Before hook calls slog.SetDefault, so concurrent invocations
// from parallel tests would race on the global logger. TestE2EApplyDown therefore
// runs serially (no t.Parallel()).
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	return cli.Execute(t.Context(), append([]string{"picolet"}, args...))
}

// TestE2EApplyDown exercises the `picolet apply` and `picolet down` CLI commands end-to-end:
// apply → verify resources → idempotent re-apply → down → verify cleanup.
//
// Runs serially (no t.Parallel) because each cli.Execute call mutates slog's default
// logger and would race with other parallel cli.Execute callers. Structural
// assertions (container running, files written, state contents) replace the
// previous log-content assertions.
//
//nolint:funlen // E2E test with intentionally sequential sub-tests
func TestE2EApplyDown(t *testing.T) {
	socketPath := podmanSocketPath(t)

	dataDir := t.TempDir()
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	setupApplyDownFleet(t, fleetDir)

	configPath := filepath.Join(dataDir, "config.yml")
	cfgContent := fmt.Sprintf("hostname: apply-host\nrootless: true\ndata_dir: %s\nsecrets_dir: %s\npodman_socket: %s\n",
		dataDir, filepath.Join(fleetDir, "secrets"), socketPath)
	require.NoError(t, os.WriteFile(configPath, []byte(cfgContent), 0o600))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	quadletDir := filepath.Join(home, ".config", "containers", "systemd", "picolet")

	// Create Podman client for verification (use Background ctx — t.Context is cancelled before Cleanup)
	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)

	// Pre-cleanup: remove stale resources from a previous interrupted run.
	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = podman.SecretRemove(preCtx, "apply_secret")
	_ = podman.ContainerRemove(preCtx, "picolet-apply-test", true)
	preCancel()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Best-effort: run down to clean managed resources.
		// Use Background-derived context (not t.Context — already cancelled at cleanup).
		_ = cli.Execute(context.Background(), []string{"picolet", "down", "--config", configPath})
		_ = podman.ContainerRemove(cleanupCtx, "picolet-apply-test", true)
		_ = podman.SecretRemove(cleanupCtx, "apply_secret")
	})

	statePath := filepath.Join(dataDir, "state.json")

	t.Run("apply", func(t *testing.T) {
		err := runCLI(t, "apply", "--host", "apply-host", "--repo-dir", fleetDir, "--config", configPath)
		require.NoError(t, err, "apply should succeed")
	})

	t.Run("verify", func(t *testing.T) {
		t.Run("container_running", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				data, err := containers.Inspect(connCtx, "picolet-apply-test", nil)
				return err == nil && data.State.Status == "running"
			}, 60*time.Second, 2*time.Second, "container picolet-apply-test should reach running state")
		})

		t.Run("secret_exists", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			exists, err := podman.SecretExists(ctx, "apply_secret")
			require.NoError(t, err)
			assert.True(t, exists, "apply_secret should exist in podman")
		})

		t.Run("quadlet_file_written", func(t *testing.T) {
			assert.FileExists(t, filepath.Join(quadletDir, "apply.container"))
		})

		t.Run("state_saved", func(t *testing.T) {
			st, err := state.NewStore(statePath).Load()
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(st.AppliedSHA, "local-"), "applied SHA should start with 'local-'")
			assert.Len(t, st.ManagedFiles, 2, "should manage container + secret")
			assert.Contains(t, st.ManagedFiles, "secret:apply_secret")
			assert.Contains(t, st.ManagedFiles, filepath.Join(quadletDir, "apply.container"))
			assert.NotZero(t, st.LastSuccessfulReconciliationAt)
		})
	})

	t.Run("idempotent", func(t *testing.T) {
		preSHA := loadAppliedSHA(t, statePath)

		err := runCLI(t, "apply", "--host", "apply-host", "--repo-dir", fleetDir, "--config", configPath)
		require.NoError(t, err, "idempotent apply should succeed")

		// No-op apply must NOT advance AppliedSHA — that's how we know nothing was applied
		// (the only place AppliedSHA changes is after a successful change-producing apply).
		postSHA := loadAppliedSHA(t, statePath)
		assert.Equal(t, preSHA, postSHA, "no-op apply must not advance AppliedSHA")
	})

	t.Run("down", func(t *testing.T) {
		err := runCLI(t, "down", "--config", configPath)
		require.NoError(t, err, "down should succeed")
	})

	t.Run("verify_cleanup", func(t *testing.T) {
		t.Run("container_gone", func(t *testing.T) {
			connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				_, err := containers.Inspect(connCtx, "picolet-apply-test", nil)
				return err != nil
			}, 30*time.Second, 2*time.Second, "container picolet-apply-test should be fully removed")
		})

		t.Run("secret_gone", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			exists, err := podman.SecretExists(ctx, "apply_secret")
			require.NoError(t, err)
			assert.False(t, exists, "apply_secret should be removed from podman")
		})

		t.Run("quadlet_file_removed", func(t *testing.T) {
			assert.NoFileExists(t, filepath.Join(quadletDir, "apply.container"))
		})

		t.Run("state_empty", func(t *testing.T) {
			st, err := state.NewStore(statePath).Load()
			require.NoError(t, err)
			assert.Empty(t, st.ManagedFiles, "all managed files should be cleared")
			assert.Empty(t, st.ServiceNames, "all service names should be cleared")
		})

		t.Run("down_idempotent", func(t *testing.T) {
			err := runCLI(t, "down", "--config", configPath)
			require.NoError(t, err, "idempotent down should succeed")
		})
	})
}

func loadAppliedSHA(t *testing.T, statePath string) string {
	t.Helper()
	st, err := state.NewStore(statePath).Load()
	require.NoError(t, err)
	return st.AppliedSHA
}
