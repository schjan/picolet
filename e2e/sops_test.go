//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/sops"
	"github.com/schjan/picolet/pkg/state"
)

const (
	sopsE2EContainer = "picolet-sops-e2e"
	sopsE2EService   = "sops-e2e.service"
	sopsE2ESecret    = "sops_e2e_password" //nolint:gosec // G101: a Podman secret name, not a credential
	sopsE2EFile      = "secrets/e2e.sops.yaml"
	// sopsE2EValue is the corpus' TEST-ONLY /db/password plaintext.
	sopsE2EValue = "hunter2-test"
)

// setupSopsFleet creates a fleet for sops-host whose container consumes the
// Podman secret sops_e2e_password, rendered by readSopsSecret from a SOPS
// file of the TEST-ONLY corpus encrypted to srv-1.
func setupSopsFleet(t *testing.T, fleetDir string) {
	t.Helper()
	files := map[string]string{
		"fleet.yml": `images:
  sops-e2e: "docker.io/library/alpine:3.23"
ports: {picolet_system_metrics: 9418}
`,
		"assignments.yml": `base: {}
roles:
  sops-test:
    paths:
      - quadlets/containers/sops-e2e.container.tmpl
    secrets:
      - secrets/` + sopsE2ESecret + `.tmpl
`,
		"hosts/sops-host/host.yml": "hostname: sops-host\nrole: sops-test\nfeatures: []\nage_recipients:\n  - " +
			testsops.Recipient(t, "srv-1") + "\n",
		"quadlets/containers/sops-e2e.container.tmpl": `[Container]
Image={{ index .Images "sops-e2e" }}
ContainerName=` + sopsE2EContainer + `
Secret=` + sopsE2ESecret + `
Exec=sleep infinity

[Install]
WantedBy=default.target
`,
		"secrets/" + sopsE2ESecret + ".tmpl": `{{ readSopsSecret "sops://` + sopsE2EFile + `#/db/password" }}`,
		sopsE2EFile:                          testsops.CorpusText(t, "values.sops.yaml"),
	}
	for name, content := range files {
		path := filepath.Join(fleetDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

// TestE2ESopsAdoption is the SOPS adoption path against real Podman and
// systemd: an Agent without an age identity fails on the sops:// ref and
// deploys nothing; the Agent started from a config with a sops: block (as
// the Agent's self-restart starts it, through the startup checks) deploys
// the decrypted value as a Podman secret its container reads; and rotating
// the file (re-encrypted, same values) restarts nothing.
//
//nolint:funlen,tparallel // E2E test with sequential sub-tests exercising the adoption path
func TestE2ESopsAdoption(t *testing.T) {
	t.Parallel()

	socketPath := podmanSocketPath(t)
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := filepath.Join(t.TempDir(), "reconciliation.lock")
	identityPath := filepath.Join(t.TempDir(), "age-identity")
	setupSopsFleet(t, fleetDir)
	require.NoError(t, os.WriteFile(identityPath, testsops.IdentityFile(t, "srv-1"), 0o600))

	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)
	systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
	require.NoError(t, err)
	quadletDir := uniqueTestQuadletDir(t)

	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = podman.ContainerRemove(preCtx, sopsE2EContainer, true)
	_ = podman.SecretRemove(preCtx, sopsE2ESecret)
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
		_ = podman.ContainerRemove(cleanupCtx, sopsE2EContainer, true)
		_ = podman.SecretRemove(cleanupCtx, sopsE2ESecret)
		systemd.Close()
	})

	metrics.Register(nil)
	store := state.NewStore(statePath)
	newAgent := func(cfg *agentcfg.Config, opts ...agent.Option) *agent.Agent {
		return agent.New(cfg, append([]agent.Option{
			agent.WithRepoPath(fleetDir),
			agent.WithFileWriter(applier.NewAtomicFileWriter()),
			agent.WithPodman(podman),
			agent.WithSystemd(systemd),
			agent.WithLockPath(lockPath),
			agent.WithStatePath(statePath),
			agent.WithQuadletDir(quadletDir),
		}, opts...)...)
	}
	baseConfig := func() *agentcfg.Config {
		return &agentcfg.Config{
			Hostname:     "sops-host",
			RepoURL:      fleetDir,
			PollInterval: time.Minute,
			SecretsDir:   t.TempDir(),
			Rootless:     true,
		}
	}
	// conn connects to Podman for t's checks.
	conn := func(t *testing.T) context.Context {
		t.Helper()
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		return connCtx
	}
	podmanSecret := func(t *testing.T, connCtx context.Context) (id, value string) {
		t.Helper()
		info, err := secrets.Inspect(connCtx, sopsE2ESecret, new(secrets.InspectOptions).WithShowSecret(true))
		require.NoError(t, err)
		return info.ID, info.SecretData
	}

	require.True(t, t.Run("no_identity_fails", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		_, err := newAgent(baseConfig()).ReconcileOnce(ctx, "sops-no-identity", state.NewState(), store)
		require.ErrorIs(t, err, sops.ErrNoIdentity)
		assert.ErrorContains(t, err, sops.NoIdentityHint)
		testsops.AssertNoPlaintext(t, err)

		exists, err := podman.SecretExists(ctx, sopsE2ESecret)
		require.NoError(t, err)
		assert.False(t, exists, "nothing is deployed")
		assert.NoFileExists(t, filepath.Join(quadletDir, "sops-e2e.container"))
	}), "the Agent without an identity must fail and deploy nothing")

	// The Agent with the sops: block starts as the self-restart starts it:
	// through the run command's startup checks, which load the identity.
	cfg := baseConfig()
	cfg.Sops = &agentcfg.SopsConfig{AgeIdentityPath: identityPath}
	startup, err := cfg.CheckStartup()
	require.NoError(t, err)
	require.NotNil(t, startup.SopsIdentities)
	a := newAgent(cfg, agent.WithStartup(startup))

	require.True(t, t.Run("identity_deploys_decrypted_secret", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		result, err := a.ReconcileOnce(ctx, "sops-adopt", state.NewState(), store)
		require.NoError(t, err)
		assert.True(t, result.HasChanges)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)

		_, value := podmanSecret(t, conn(t))
		assert.Equal(t, sopsE2EValue, value, "the Podman secret holds the decrypted value")
		st, err := store.Load()
		require.NoError(t, err)
		assert.True(t, st.ManagedFiles["secret:"+sopsE2ESecret].SopsValues, "the state marks the secret as SOPS-backed")
	}), "the Agent with an identity must deploy the secret")

	require.True(t, t.Run("container_reads_secret", func(t *testing.T) {
		connCtx := conn(t)
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, sopsE2EContainer, nil)
			return err == nil && data.State.Status == "running"
		}, 60*time.Second, 2*time.Second, "container %s should reach running state", sopsE2EContainer)
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		require.Eventually(t, func() bool {
			status, err := systemd.GetUnitStatus(ctx, sopsE2EService)
			return err == nil && status.ActiveState == "active"
		}, 60*time.Second, 2*time.Second, "%s should reach active state", sopsE2EService)
		out, err := exec.CommandContext(ctx, "podman", "exec", sopsE2EContainer, "cat", "/run/secrets/"+sopsE2ESecret).Output()
		require.NoError(t, err)
		assert.Equal(t, sopsE2EValue, string(out))
	}), "the container must run with the secret")

	t.Run("rotation_restarts_nothing", func(t *testing.T) {
		connCtx := conn(t)
		before, err := containers.Inspect(connCtx, sopsE2EContainer, nil)
		require.NoError(t, err)
		secretID, _ := podmanSecret(t, connCtx)

		// The corpus' re-encrypted twin: same values, new data key and
		// ciphertexts, as `sops rotate` or `sops updatekeys` leave a file.
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, sopsE2EFile),
			[]byte(testsops.CorpusText(t, "values-reencrypted.sops.yaml")), 0o600))
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		st, err := store.Load()
		require.NoError(t, err)
		result, err := a.ReconcileOnce(ctx, "sops-rotate", st, store)
		require.NoError(t, err)
		assert.False(t, result.HasChanges, "a re-encrypted file with the same values changes nothing")
		assert.Nil(t, result.ApplyResult, "no apply runs")

		gotID, value := podmanSecret(t, connCtx)
		assert.Equal(t, secretID, gotID, "the Podman secret is not recreated")
		assert.Equal(t, sopsE2EValue, value)
		after, err := containers.Inspect(connCtx, sopsE2EContainer, nil)
		require.NoError(t, err)
		assert.Equal(t, before.State.StartedAt, after.State.StartedAt, "the container is not restarted")
		status, err := systemd.GetUnitStatus(ctx, sopsE2EService)
		require.NoError(t, err)
		assert.Equal(t, "active", status.ActiveState)
	})
}
