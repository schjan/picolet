//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// setupApplyDownFleet creates a minimal fleet repo in fleetDir: a container
// Quadlet, a raw systemd unit, a managed data file and a secret.
func setupApplyDownFleet(t *testing.T, fleetDir string) {
	t.Helper()

	files := map[string]string{
		"fleet.yml": `images:
  apply: "` + applyImage + `"
ports: {picolet_system_metrics: 9418}
`,
		"assignments.yml": `base: {}
roles:
  apply:
    paths:
      - quadlets/containers/apply.container.tmpl
      - systemd/apply-raw.service
      - files/apply/data.txt
    secrets:
      - secrets/apply_secret.txt
`,
		filepath.Join("hosts", "apply-host", "host.yml"): `hostname: apply-host
external_hostname: apply-host.local
role: apply
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
		filepath.Join("systemd", "apply-raw.service"): `[Unit]
Description=picolet e2e raw unit

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true

[Install]
WantedBy=default.target
`,
		filepath.Join("files", "apply", "data.txt"):  "apply-test-data\n",
		filepath.Join("secrets", "apply_secret.txt"): "apply-test-secret-data\n",
	}

	for name, content := range files {
		path := filepath.Join(fleetDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

const applyImage = "docker.io/library/alpine:3.23"

// runCLI invokes the picolet CLI in-process via cli.Execute. The args slice must
// start with the subcommand (no leading "picolet"; runCLI adds it).
//
// cli.Execute's Before hook calls slog.SetDefault, so a scenario never runs
// two invocations concurrently; each scenario has its own test process.
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	return cli.Execute(t.Context(), append([]string{"picolet"}, args...))
}

// TestE2EApplyDown exercises the `picolet apply` and `picolet down` CLI commands end-to-end:
// apply → verify resources and their discovery by the user manager →
// idempotent re-apply → down → verify cleanup.
//
// Its runtime is disposable, so it leaves cleanup to the runner; down itself
// is under test and must succeed.
//
//nolint:funlen,paralleltest // E2E test with intentionally sequential sub-tests; one scenario per process
func TestE2EApplyDown(t *testing.T) {
	rt := isolatedRuntime(t)
	socketPath := rt.PodmanSocket
	requireImage(t, applyImage)

	dataDir := t.TempDir()
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	setupApplyDownFleet(t, fleetDir)

	configPath := filepath.Join(dataDir, "config.yml")
	cfgContent := fmt.Sprintf("hostname: apply-host\nrootless: true\ndata_dir: %s\nsecrets_dir: %s\npodman_socket: %s\n",
		dataDir, filepath.Join(fleetDir, "secrets"), socketPath)
	require.NoError(t, os.WriteFile(configPath, []byte(cfgContent), 0o600))

	quadletFile := filepath.Join(rt.QuadletDir, "apply.container")
	rawUnit := filepath.Join(rt.SystemdDir, "apply-raw.service")
	dataFile := filepath.Join(rt.DataDir, "files", "apply", "data.txt")

	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)

	statePath := filepath.Join(dataDir, "state.json")

	require.True(t, t.Run("apply", func(t *testing.T) {
		err := runCLI(t, "apply", "--host", "apply-host", "--repo-dir", fleetDir, "--config", configPath)
		require.NoError(t, err, "apply should succeed")
	}), "apply failed")

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

		t.Run("files_written", func(t *testing.T) {
			assert.FileExists(t, quadletFile)
			assert.FileExists(t, rawUnit)
			data, err := os.ReadFile(dataFile)
			require.NoError(t, err)
			assert.Equal(t, "apply-test-data\n", string(data))
		})

		t.Run("quadlet_service_generated", func(t *testing.T) {
			assertGeneratedFrom(t, rt, "apply.service", quadletFile)
			assert.Equal(t, "active", unitProperty(t, "apply.service", "ActiveState"))
		})

		t.Run("raw_unit_loaded", func(t *testing.T) {
			assert.Equal(t, rawUnit, unitProperty(t, "apply-raw.service", "FragmentPath"),
				"the raw unit should load from the user's standard unit dir")
			assert.Equal(t, "enabled", unitProperty(t, "apply-raw.service", "UnitFileState"))
			assert.Equal(t, "active", unitProperty(t, "apply-raw.service", "ActiveState"))
		})

		t.Run("state_saved", func(t *testing.T) {
			st, err := state.NewStore(statePath).Load()
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(st.AppliedSHA, "local-"), "applied SHA should start with 'local-'")
			assert.ElementsMatch(t, []string{"secret:apply_secret", quadletFile, rawUnit, dataFile}, slices.Collect(maps.Keys(st.ManagedFiles)))
			assertStateInRuntime(t, rt, st)
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

	require.True(t, t.Run("down", func(t *testing.T) {
		err := runCLI(t, "down", "--config", configPath)
		require.NoError(t, err, "down should succeed")
	}), "down failed")

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

		t.Run("files_removed", func(t *testing.T) {
			assert.NoFileExists(t, quadletFile)
			assert.NoFileExists(t, rawUnit)
			assert.NoFileExists(t, dataFile)
		})

		t.Run("units_unloaded", func(t *testing.T) {
			assert.Equal(t, "not-found", unitProperty(t, "apply.service", "LoadState"),
				"the generated service should be gone after down's daemon-reload")
			assert.Equal(t, "not-found", unitProperty(t, "apply-raw.service", "LoadState"))
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
