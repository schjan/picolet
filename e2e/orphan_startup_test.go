//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/cli"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/state"
)

const (
	orphanImage   = "docker.io/library/alpine:3.23"
	orphanSecret  = "e2e_orphan_secret"  //nolint:gosec // G101: a Podman secret name, not a credential
	foreignSecret = "e2e_foreign_secret" //nolint:gosec // G101: a Podman secret name, not a credential
	orphanService = "orphan-picolet.service"
)

// orphanFixtures are the orphans plantOrphans leaves in rt: Managed-File
// look-alikes the state does not record, one per scanned destination.
func orphanFixtures(rt scenarioRuntime) []string {
	return []string{
		filepath.Join(rt.QuadletDir, "orphan.container"),
		filepath.Join(rt.SystemdDir, orphanService),
		filepath.Join(rt.DataDir, "files", "orphan", "orphan.txt"),
	}
}

// plantOrphans writes the orphan fixtures, creates a picolet-labelled
// secret and starts the marked raw unit. TestE2EAgentStartupOrphanScan and
// TestE2EOrphanNeighbour plant the same names, each in its own runtime.
func plantOrphans(ctx context.Context, t *testing.T, rt scenarioRuntime, podman *applier.SocketPodmanClient, systemd *applier.DBusSystemdManager) {
	t.Helper()
	contents := []string{
		"[Container]\nImage=localhost/picolet-e2e-ghost\n",
		config.PicoletMarker + "\n[Unit]\nDescription=picolet e2e orphan\n\n[Service]\nExecStart=/bin/sleep infinity\n",
		"orphan data\n",
	}
	for i, path := range orphanFixtures(rt) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(contents[i]), 0o600))
	}
	// SecretCreate labels the secret managed-by=picolet.
	require.NoError(t, podman.SecretCreate(ctx, orphanSecret, []byte("orphan-secret"), false))
	require.NoError(t, systemd.DaemonReload(ctx))
	require.NoError(t, systemd.StartUnit(ctx, orphanService))
}

func startupClients(t *testing.T, rt scenarioRuntime) (*applier.SocketPodmanClient, *applier.DBusSystemdManager) {
	t.Helper()
	podman, err := applier.NewSocketPodmanClient(context.Background(), rt.PodmanSocket)
	require.NoError(t, err)
	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)
	t.Cleanup(systemd.Close)
	return podman, systemd
}

// TestE2EOrphanNeighbour is the neighbour of TestE2EAgentStartupOrphanScan:
// the runner runs its plant subtest before the other scenarios and its verify
// subtest after them, in one runtime. Its fixtures carry the names the
// startup scan removes in its own runtime, and must survive it.
//
//nolint:paralleltest // one scenario per process; the runner sequences the subtests
func TestE2EOrphanNeighbour(t *testing.T) {
	rt := isolatedRuntime(t)
	podman, systemd := startupClients(t, rt)

	t.Run("plant", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		plantOrphans(ctx, t, rt, podman, systemd)
	})

	t.Run("verify", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		for _, path := range orphanFixtures(rt) {
			assert.FileExists(t, path, "another scenario's orphan scan must not reach this runtime")
		}
		exists, err := podman.SecretExists(ctx, orphanSecret)
		require.NoError(t, err)
		assert.True(t, exists, "the labelled secret must survive another runtime's scan")
		st, err := systemd.GetUnitStatus(ctx, orphanService)
		require.NoError(t, err)
		assert.Equal(t, "active", st.ActiveState, "the sentinel service must keep running")
	})
}

// setupStartupFleet commits a fleet for startup-host (a container, a data
// file and a secret) and returns its work tree, a bare clone to poll, and
// the clone's HEAD.
func setupStartupFleet(t *testing.T) (workDir, bareDir, head string) {
	t.Helper()
	workDir = filepath.Join(t.TempDir(), "fleet")
	bareDir = filepath.Join(t.TempDir(), "fleet.git")
	files := map[string]string{
		"fleet.yml": "images:\n  keep: \"" + orphanImage + "\"\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml": `base: {}
roles:
  startup:
    paths:
      - quadlets/containers/keep.container.tmpl
      - files/keep/keep.txt
    secrets:
      - secrets/keep_secret.txt
`,
		"hosts/startup-host/host.yml": "hostname: startup-host\nrole: startup\nfeatures: []\n",
		"quadlets/containers/keep.container.tmpl": `[Container]
Image={{ index .Images "keep" }}
ContainerName=picolet-e2e-keep
Secret=keep_secret
Exec=sleep infinity

[Install]
WantedBy=default.target
`,
		"files/keep/keep.txt":     "kept\n",
		"secrets/keep_secret.txt": "keep-secret\n",
	}
	for name, content := range files {
		path := filepath.Join(workDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	sha, err := wt.Commit("fleet", &git.CommitOptions{Author: &object.Signature{Name: "e2e", Email: "e2e@example.invalid", When: time.Now()}})
	require.NoError(t, err)
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return workDir, bareDir, sha.String()
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // a TCP listener has a TCP address
	require.NoError(t, l.Close())
	return port
}

// waitHealthy polls the Agent's /health until its first reconciliation
// succeeded, or until run returned.
func waitHealthy(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	deadline := time.After(120 * time.Second)
	for {
		select {
		case err := <-done:
			require.FailNow(t, "picolet run returned before it became healthy", "error: %v", err)
		case <-deadline:
			require.FailNow(t, "picolet run did not become healthy in 120s")
		case <-time.After(time.Second):
		}
		resp, err := http.Get("http://" + addr + "/health") //nolint:noctx // bounded by the loop's deadline
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
}

func metricsText(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics") //nolint:noctx // local test server
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// TestE2EAgentStartupOrphanScan drives the orphan scan through the Agent's
// real startup: `picolet apply` deploys a fleet, orphans and sentinels are
// planted next to it, and `picolet run` (the production command tree) must
// remove the orphans at startup, keep the Managed Files and the unmarked or
// unlabelled sentinels, and reconcile. TestE2EOrphanNeighbour checks that a
// neighbouring runtime's look-alikes survive.
//
//nolint:paralleltest,funlen // one scenario per process; sequential staged exercise
func TestE2EAgentStartupOrphanScan(t *testing.T) {
	rt := isolatedRuntime(t)
	requireImage(t, orphanImage)
	podman, systemd := startupClients(t, rt)

	workDir, bareDir, head := setupStartupFleet(t)
	dataDir := t.TempDir()
	addr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	configPath := filepath.Join(dataDir, "config.yml")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `hostname: startup-host
repo_url: %s
repo_branch: master
poll_interval: 1h
listen_addr: %s
rootless: true
data_dir: %s
secrets_dir: %s
podman_socket: %s
prune_images: false
`, bareDir, addr, dataDir, filepath.Join(workDir, "secrets"), rt.PodmanSocket), 0o600))
	statePath := filepath.Join(dataDir, "state.json")

	keepQuadlet := filepath.Join(rt.QuadletDir, "keep.container")
	keepData := filepath.Join(rt.DataDir, "files", "keep", "keep.txt")
	foreignUnit := filepath.Join(rt.SystemdDir, "foreign.service")

	require.True(t, t.Run("apply", func(t *testing.T) {
		require.NoError(t, runCLI(t, "apply", "--host", "startup-host", "--repo-dir", workDir, "--config", configPath))
		assertGeneratedFrom(t, rt, "keep.service", keepQuadlet)
	}), "apply failed")

	require.True(t, t.Run("plant", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		plantOrphans(ctx, t, rt, podman, systemd)
		// Sentinels: an unmarked raw unit (running) and an unlabelled secret.
		require.NoError(t, os.WriteFile(foreignUnit,
			[]byte("[Unit]\nDescription=picolet e2e foreign\n\n[Service]\nExecStart=/bin/sleep infinity\n"), 0o600))
		require.NoError(t, systemd.DaemonReload(ctx))
		require.NoError(t, systemd.StartUnit(ctx, "foreign.service"))
		cmd := exec.CommandContext(ctx, "podman", "secret", "create", foreignSecret, "-")
		cmd.Stdin = strings.NewReader("foreign")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "podman secret create: %s", out)
	}), "planting failed")

	require.True(t, t.Run("run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- cli.Execute(ctx, []string{"picolet", "run", "--config", configPath}) }()
		defer func() {
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err, "picolet run should shut down cleanly")
			case <-time.After(30 * time.Second):
				t.Error("picolet run did not shut down in 30s")
			}
		}()
		waitHealthy(t, addr, done)
		m := metricsText(t, addr)
		assert.Contains(t, m, `picolet_orphans_removed_total{type="file"} 3`)
		assert.Contains(t, m, `picolet_orphans_removed_total{type="secret"} 1`)
	}), "picolet run failed")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	t.Run("orphans_removed", func(t *testing.T) {
		for _, path := range orphanFixtures(rt) {
			assert.NoFileExists(t, path)
		}
		exists, err := podman.SecretExists(ctx, orphanSecret)
		require.NoError(t, err)
		assert.False(t, exists, "the picolet-labelled orphan secret should be removed")
	})

	t.Run("sentinels_kept", func(t *testing.T) {
		assert.FileExists(t, foreignUnit, "an unmarked unit is not picolet's")
		assert.Equal(t, "active", unitProperty(t, "foreign.service", "ActiveState"))
		exists, err := podman.SecretExists(ctx, foreignSecret)
		require.NoError(t, err)
		assert.True(t, exists, "an unlabelled secret is not picolet's")
	})

	t.Run("managed_files_kept", func(t *testing.T) {
		assert.FileExists(t, keepQuadlet)
		assert.FileExists(t, keepData)
		exists, err := podman.SecretExists(ctx, "keep_secret")
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, "active", unitProperty(t, "keep.service", "ActiveState"))
	})

	t.Run("reconciled", func(t *testing.T) {
		st, err := state.NewStore(statePath).Load()
		require.NoError(t, err)
		assert.Contains(t, st.ManagedFiles, keepQuadlet)
		assert.Contains(t, st.ManagedFiles, keepData)
		assertStateInRuntime(t, rt, st)
		assert.Empty(t, st.FailedSHA, "the first tick after startup should succeed (head %s)", head)
	})
}
