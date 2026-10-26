package agent

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/state"
)

const (
	selfConfigFile   = "services/picolet-system/secrets/picolet_system_config.yml"
	selfConfigSecret = "picolet_system_config"
	selfUnit         = "picolet-system.service"
	// configChangedLog is what the Agent logs when it restarts to load its
	// changed config.
	configChangedLog = "Agent config changed, restarting to load it"
)

// selfRestartConfig is a config for the Agent of test-host polling bareDir,
// with its secrets in secretsDir, followed by configExtra.
func selfRestartConfig(bareDir, secretsDir, configExtra string) string {
	return "hostname: test-host\nrepo_url: " + bareDir + "\nrepo_branch: master\npoll_interval: 1s\nsecrets_dir: " + secretsDir + "\nprune_images: false\n" + configExtra
}

// restartsOf counts the restarts of unit the Agent asked for.
func (r *applyRecord) restartsOf(unit string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(slices.DeleteFunc(slices.Clone(r.restarts), func(u string) bool { return u != unit }))
}

// TestAgentRestartsItselfToLoadChangedConfig is the SOPS adoption path: the
// Agent runs with a config without a sops: block; a commit that only adds
// the block to the Agent's Fleet-rendered config makes the Agent restart its
// own unit (Podman copies the config secret into the container only when it
// creates it), and the Agent that restart starts loads the identity, so the
// next commit's sops:// ref deploys its decrypted value.
func TestAgentRestartsItselfToLoadChangedConfig(t *testing.T) {
	t.Parallel()
	identity := writeIdentityFile(t, "srv-1")
	secretsDir := t.TempDir()
	unit := agentBundleUnit("")
	bareDir, _ := initFleetRepo(t, agentBundleFiles(unit, "picolet_system_config.yml", "hostname: test-host\n"))
	pushToTestRepo(t, bareDir, map[string]string{selfConfigFile: selfRestartConfig(bareDir, secretsDir, "")})

	cfg, err := agentcfg.Parse([]byte(selfRestartConfig(bareDir, secretsDir, "")))
	require.NoError(t, err)
	logger, logs := testlog.New()
	h := newConfiguredLoopHarness(t, cfg, WithDataDir(t.TempDir()), WithLogger(logger))
	h.mocks = mocksSelfConfig
	h.seedState(t, true, nil)

	// Commit 1: only the config secret changes, adding the sops: block.
	pushToTestRepo(t, bareDir, map[string]string{
		selfConfigFile: selfRestartConfig(bareDir, secretsDir, "sops:\n  age_identity_path: "+identity+"\n"),
	})
	require.NoError(t, h.tick(t))
	require.Contains(t, h.secret(selfConfigSecret), "age_identity_path")
	assert.Eventually(t, func() bool { return h.restartsOf(selfUnit) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the Agent restarts its own unit")
	assert.True(t, logs.Has(slog.LevelInfo, configChangedLog))

	// The restart: a new Agent starts from the config the container now
	// holds, through the run command's startup checks.
	restarted, err := agentcfg.Parse([]byte(h.secret(selfConfigSecret)))
	require.NoError(t, err)
	startup, err := restarted.CheckStartup()
	require.NoError(t, err)
	require.NotNil(t, startup.SopsIdentities)
	h2 := newConfiguredLoopHarness(t, restarted, WithDataDir(t.TempDir()), WithStartup(startup), WithStatePath(h.statePath))
	h2.statePath, h2.store = h.statePath, h.store
	h2.mocks = mocksSelfConfig
	require.NoError(t, h2.a.setupSops())

	// Commit 2: a sops:// ref.
	pushToTestRepo(t, bareDir, map[string]string{
		"secrets/db.sops.yaml": testsops.CorpusText(t, "values.sops.yaml"),
		"assignments.yml":      "base:\n  services:\n    - picolet-system\n  secrets:\n    - \"sops://secrets/db.sops.yaml#/db/password\"\n",
	})
	require.NoError(t, h2.tick(t))
	assert.Equal(t, "hunter2-test", h2.secret("db_password"))
	assert.Zero(t, h2.restartsOf(selfUnit), "a commit that leaves the Agent config alone does not restart the Agent")
}

// A commit that leaves the Agent's config secret and unit alone does not
// restart the Agent, nor does a dry run of one that changes the config. The
// config is the secret mounted where the Agent reads it, also by PodmanArgs=
// --secret or at the path PICOLET_CONFIG names.
func TestAgentRestartsItselfOnlyForAppliedConfigChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		unit        string // the Agent quadlet; empty: agentBundleUnit("")
		files       map[string]string
		dryRun      bool
		wantRestart bool
	}{
		{name: "config changed", files: map[string]string{selfConfigFile: "hostname: test-host\nrepo_url: https://example.com/other.git\n"}, wantRestart: true},
		{name: "other secret changed", files: map[string]string{"services/picolet-system/secrets/other": "x\n"}},
		{
			name:        "config mounted by PodmanArgs= changed",
			unit:        "[Container]\nImage=ghcr.io/schjan/picolet:latest\nPodmanArgs=--secret picolet_system_config,target=/etc/picolet/config.yml\n",
			files:       map[string]string{selfConfigFile: "hostname: test-host\nrepo_url: https://example.com/other.git\n"},
			wantRestart: true,
		},
		{
			name: "config at PICOLET_CONFIG changed",
			unit: "[Container]\nImage=ghcr.io/schjan/picolet:latest\nEnvironment=PICOLET_CONFIG=/etc/picolet/agent.yml\n" +
				"Secret=picolet_system_config,target=/etc/picolet/agent.yml\n",
			files:       map[string]string{selfConfigFile: "hostname: test-host\nrepo_url: https://example.com/other.git\n"},
			wantRestart: true,
		},
		{name: "config changed, dry run", files: map[string]string{selfConfigFile: "hostname: test-host\nrepo_url: https://example.com/other.git\n"}, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unit := tc.unit
			if unit == "" {
				unit = agentBundleUnit("")
			}
			bareDir, _ := initFleetRepo(t, agentBundleFiles(unit, "picolet_system_config.yml",
				"hostname: test-host\nrepo_url: https://example.com/fleet.git\n"))
			logger, logs := testlog.New()
			h := newLoopHarness(t, bareDir, WithDryRun(tc.dryRun), WithLogger(logger))
			h.mocks = mocksSelfConfig
			h.seedState(t, true, nil)
			for path, content := range tc.files {
				writeTestFile(t, h.repoDir, path, content)
			}
			result, err := h.a.ReconcileOnce(t.Context(), "next-sha", mustLoadState(t, h), h.store)
			require.NoError(t, err)
			require.True(t, result.HasChanges)
			assert.Equal(t, tc.wantRestart, slices.Contains(result.ApplyResult.RestartedUnits, selfUnit))
			assert.Equal(t, tc.wantRestart, logs.Has(slog.LevelInfo, configChangedLog))
		})
	}
}

func mustLoadState(t *testing.T, h *loopHarness) *state.State {
	t.Helper()
	st, err := h.store.Load()
	require.NoError(t, err)
	return st
}
