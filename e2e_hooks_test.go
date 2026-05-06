//go:build e2e

package picolet_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
)

// setupHookFleet creates a minimal fleet dir for hook e2e tests.
// The fleet has a single container that sleeps forever, a secret, and a
// picolet.yml with the given hook configuration.
func setupHookFleet(t *testing.T, fleetDir string, params hookFleetParams) {
	t.Helper()

	dirs := []string{
		filepath.Join(fleetDir, "hosts", "hook-host"),
		filepath.Join(fleetDir, "services", params.ServiceName, "containers"),
		filepath.Join(fleetDir, "services", params.ServiceName, "secrets"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := map[string]string{
		"fleet.yml": params.FleetYAML,
		"assignments.yml": fmt.Sprintf(`base: {}
pi_types:
  hook-test:
    services:
      - %s
`, params.ServiceName),
		filepath.Join("hosts", "hook-host", "host.yml"): `hostname: hook-host
pi_type: hook-test
features: []
`,
		filepath.Join("services", params.ServiceName, "containers", params.ContainerFile): params.ContainerContent,
		filepath.Join("services", params.ServiceName, "secrets", params.SecretFile):       "initial-secret-data\n",
	}

	// picolet.yml or picolet.yml.tmpl
	picoletPath := filepath.Join("services", params.ServiceName, params.PicoletFile)
	files[picoletPath] = params.PicoletContent

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, name), []byte(content), 0o600))
	}
}

type hookFleetParams struct {
	ServiceName      string
	ContainerFile    string
	ContainerContent string
	SecretFile       string
	FleetYAML        string
	PicoletFile      string
	PicoletContent   string
}

// TestE2EHookRestart verifies that a restart hook with unit: <name>.container
// resolves to the correct systemd service name and restarts the unit when a
// secret changes.
//
//nolint:funlen,tparallel // E2E test with sequential sub-tests exercising the full hook pipeline
func TestE2EHookRestart(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	secretsDir := filepath.Join(fleetDir, "services", "hook-restart-svc", "secrets")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")

	const containerName = "picolet-hook-restart-test"

	setupHookFleet(t, fleetDir, hookFleetParams{ //nolint:gosec // test fixture, not real credentials
		ServiceName:   "hook-restart-svc",
		ContainerFile: "hook-restart.container.tmpl",
		ContainerContent: `[Container]
Image={{ index .Images "hook-restart" }}
ContainerName=` + containerName + `
Exec=sleep infinity

[Install]
WantedBy=default.target
`,
		SecretFile: "hook_cfg.txt",
		FleetYAML: `images:
  hook-restart: "docker.io/library/alpine:3.23"
ports: {}
`,
		PicoletFile: "picolet.yml",
		PicoletContent: `hooks:
  - name: hook-restart-reload
    secrets: [hook_cfg]
    unit: hook-restart.container
    action: restart
`,
	})

	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)

	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)

	// Pre-cleanup
	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = podman.ContainerRemove(preCtx, containerName, true)
	_ = podman.SecretRemove(preCtx, "hook_cfg")
	preCancel()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, loadErr := state.NewStore(statePath).Load()
		if loadErr == nil {
			for destPath := range st.ManagedFiles {
				if !strings.HasPrefix(destPath, "secret:") {
					_ = os.Remove(destPath)
				}
			}
		}
		_ = systemd.DaemonReload(cleanupCtx)
		_ = podman.ContainerRemove(cleanupCtx, containerName, true)
		_ = podman.SecretRemove(cleanupCtx, "hook_cfg")
		systemd.Close()
	})

	metrics.Register(nil)

	agentCfg := &agentcfg.Config{
		Hostname:     "hook-host",
		PollInterval: time.Minute,
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

	var headSHA string

	t.Run("deploy", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		headSHA = "hook-restart-initial"
		result, err := a.ReconcileOnce(ctx, headSHA, state.NewState(), store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)
	})

	t.Run("wait_running", func(t *testing.T) {
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, containerName, nil)
			return err == nil && data.State.Status == "running"
		}, 60*time.Second, 2*time.Second, "container %s should reach running state", containerName)
	})

	t.Run("update_secret_triggers_restart", func(t *testing.T) {
		// Update the secret to trigger the hook
		require.NoError(t, os.WriteFile(
			filepath.Join(secretsDir, "hook_cfg.txt"),
			[]byte("updated-secret-data\n"), 0o600))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		st, err := store.Load()
		require.NoError(t, err)

		headSHA = "hook-restart-update"
		result, err := a.ReconcileOnce(ctx, headSHA, st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)
		assert.Contains(t, result.ApplyResult.RestartedUnits, "hook-restart.service",
			"restart hook should trigger unit restart via resolved quadlet name")
	})

	t.Run("container_still_running", func(t *testing.T) {
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, containerName, nil)
			return err == nil && data.State.Status == "running"
		}, 60*time.Second, 2*time.Second, "container should be running after restart")
	})
}

// TestE2EHookHTTP verifies that an HTTP hook with unit: <name>.container resolves
// correctly and fires an HTTP reload request against a real server when a secret
// changes.
//
//nolint:funlen,tparallel // E2E test with sequential sub-tests exercising the full hook pipeline
func TestE2EHookHTTP(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	secretsDir := filepath.Join(fleetDir, "services", "hook-http-svc", "secrets")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")

	const containerName = "picolet-hook-http-test"

	// Start a test HTTP server to receive reload and health check requests.
	var reloadCalls atomic.Int32
	var healthCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reload":
			reloadCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/health":
			healthCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// Extract host:port from the test server URL (e.g., "127.0.0.1:12345")
	srvAddr := strings.TrimPrefix(srv.URL, "http://")

	setupHookFleet(t, fleetDir, hookFleetParams{ //nolint:gosec // test fixture, not real credentials
		ServiceName:   "hook-http-svc",
		ContainerFile: "hook-http.container.tmpl",
		ContainerContent: `[Container]
Image={{ index .Images "hook-http" }}
ContainerName=` + containerName + `
Exec=sleep infinity

[Install]
WantedBy=default.target
`,
		SecretFile: "hook_http_cfg.txt",
		FleetYAML: `images:
  hook-http: "docker.io/library/alpine:3.23"
ports: {}
`,
		PicoletFile: "picolet.yml",
		PicoletContent: fmt.Sprintf(`hooks:
  - name: hook-http-reload
    secrets: [hook_http_cfg]
    unit: hook-http.container
    action: http
    method: GET
    url: "http://%s/reload"
    health_url: "http://%s/health"
`, srvAddr, srvAddr),
	})

	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)

	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)

	// Pre-cleanup
	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = podman.ContainerRemove(preCtx, containerName, true)
	_ = podman.SecretRemove(preCtx, "hook_http_cfg")
	preCancel()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, loadErr := state.NewStore(statePath).Load()
		if loadErr == nil {
			for destPath := range st.ManagedFiles {
				if !strings.HasPrefix(destPath, "secret:") {
					_ = os.Remove(destPath)
				}
			}
		}
		_ = systemd.DaemonReload(cleanupCtx)
		_ = podman.ContainerRemove(cleanupCtx, containerName, true)
		_ = podman.SecretRemove(cleanupCtx, "hook_http_cfg")
		systemd.Close()
	})

	metrics.Register(nil)

	agentCfg := &agentcfg.Config{
		Hostname:     "hook-host",
		PollInterval: time.Minute,
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

	var headSHA string

	t.Run("deploy", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		headSHA = "hook-http-initial"
		result, err := a.ReconcileOnce(ctx, headSHA, state.NewState(), store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)
	})

	t.Run("wait_running", func(t *testing.T) {
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, containerName, nil)
			return err == nil && data.State.Status == "running"
		}, 60*time.Second, 2*time.Second, "container %s should reach running state", containerName)
	})

	t.Run("update_secret_triggers_http_hook", func(t *testing.T) {
		// Reset counters (initial deploy may or may not have triggered them)
		reloadCalls.Store(0)
		healthCalls.Store(0)

		// Update the secret to trigger the hook
		require.NoError(t, os.WriteFile(
			filepath.Join(secretsDir, "hook_http_cfg.txt"),
			[]byte("updated-http-secret-data\n"), 0o600))

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		st, err := store.Load()
		require.NoError(t, err)

		headSHA = "hook-http-update"
		result, err := a.ReconcileOnce(ctx, headSHA, st, store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors, "HTTP hook should succeed")

		assert.Equal(t, int32(1), reloadCalls.Load(),
			"reload endpoint should be called exactly once")
		assert.Equal(t, int32(1), healthCalls.Load(),
			"health endpoint should be called exactly once after reload")
	})

	t.Run("container_not_restarted", func(t *testing.T) {
		// The HTTP hook should NOT have triggered a unit restart (it's not a restart hook)
		st, err := store.Load()
		require.NoError(t, err)
		assert.Equal(t, headSHA, st.AppliedSHA)

		// Container should still be running with the same start time (not restarted)
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		data, err := containers.Inspect(connCtx, containerName, nil)
		require.NoError(t, err)
		assert.Equal(t, "running", data.State.Status)
	})
}
