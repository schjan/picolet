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

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

// podmanBuildTags are required to compile cmd/picolet against the Podman bindings.
const podmanBuildTags = "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper"

func buildPicoletBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "picolet")
	cmd := exec.Command("go", "build", "-tags", podmanBuildTags, "-o", bin, "./cmd/picolet")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building picolet binary: %s", out)
	return bin
}

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

// TestE2EApplyDown exercises the `picolet apply` and `picolet down` CLI commands end-to-end:
// apply → verify resources → idempotent re-apply → down → verify cleanup.
//
//nolint:funlen,tparallel // E2E test with intentionally sequential sub-tests
func TestE2EApplyDown(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	bin := buildPicoletBinary(t)

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
		// Best-effort: run down to clean managed resources
		downCmd := exec.Command(bin, "down", "--config", configPath)
		_ = downCmd.Run()
		_ = podman.ContainerRemove(cleanupCtx, "picolet-apply-test", true)
		_ = podman.SecretRemove(cleanupCtx, "apply_secret")
	})

	statePath := filepath.Join(dataDir, "state.json")

	t.Run("apply", func(t *testing.T) {
		cmd := exec.Command(bin, "apply",
			"--host", "apply-host",
			"--repo-dir", fleetDir,
			"--config", configPath)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "apply failed: %s", out)
		assert.Contains(t, string(out), "apply complete")
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
		cmd := exec.Command(bin, "apply",
			"--host", "apply-host",
			"--repo-dir", fleetDir,
			"--config", configPath)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "idempotent apply failed: %s", out)
		assert.Contains(t, string(out), "no changes to apply")
	})

	t.Run("down", func(t *testing.T) {
		cmd := exec.Command(bin, "down", "--config", configPath)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "down failed: %s", out)
		assert.Contains(t, string(out), "down complete")
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
			cmd := exec.Command(bin, "down", "--config", configPath)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "idempotent down failed: %s", out)
			assert.Contains(t, string(out), "nothing to tear down")
		})
	})
}
