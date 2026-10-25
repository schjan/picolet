package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// Each Agent records its secret-provider series in its own status store, so
// two Agents never see each other's counts.
func TestAgentsRecordSecretSyncIndependently(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"}
	a1 := newTestAgent(t, cfg)
	a2 := newTestAgent(t, cfg)

	refFiles := []resolver.ResolvedFile{{SrcPath: "op://vault/item/a"}, {SrcPath: "op://vault/item/b"}, {SrcPath: "plain"}}
	isRef := func(p string) bool { return strings.HasPrefix(p, "op://") }

	n := a1.recordSecretsManaged(resolver.ProviderOnePassword, true, isRef, refFiles)
	a1.recordProviderSyncSuccess(resolver.ProviderOnePassword, n)
	a1.recordProviderSyncSuccess(resolver.ProviderOnePassword, n)
	a2.recordSecretsManaged(resolver.ProviderOnePassword, true, isRef, refFiles[2:])

	require.NoError(t, testutil.CollectAndCompare(metrics.NewSecretProviderCollector(a1.statusStore), strings.NewReader(`
# HELP picolet_secret_sync_total Successful secret-provider sync attempts, per provider. Failures are counted on picolet_reconciliation_total{result="failure"}.
# TYPE picolet_secret_sync_total counter
picolet_secret_sync_total{provider="onepassword"} 2
# HELP picolet_secrets_managed_count Number of direct provider-backed secret assignments currently managed, per provider.
# TYPE picolet_secrets_managed_count gauge
picolet_secrets_managed_count{provider="onepassword"} 2
`), "picolet_secret_sync_total", "picolet_secrets_managed_count"))

	require.NoError(t, testutil.CollectAndCompare(metrics.NewSecretProviderCollector(a2.statusStore), strings.NewReader(`
# HELP picolet_secrets_managed_count Number of direct provider-backed secret assignments currently managed, per provider.
# TYPE picolet_secrets_managed_count gauge
picolet_secrets_managed_count{provider="onepassword"} 0
`), "picolet_secret_sync_total", "picolet_secrets_managed_count"))
}

// The failed-SHA gate reads the persisted failure count into the Agent's own
// store; a second Agent's gauge is unaffected.
func TestAgentsRecordFailedSHAGaugeIndependently(t *testing.T) {
	t.Parallel()
	gated, gatedLogs := tickWithFailedSHA(t, 3)
	fresh, freshLogs := tickWithFailedSHA(t, 0)

	assert.Equal(t, 3, gated.statusStore.FailedSHACount())
	assert.Equal(t, 0, fresh.statusStore.FailedSHACount())
	assert.InDelta(t, 3.0, testutil.ToFloat64(metrics.NewFailedSHACollector(gated.statusStore)), 0)
	assert.True(t, gatedLogs.Has(slog.LevelWarn, "reconciliation: noop"),
		"the gate's warning goes to the Agent's logger")
	// The fixture's role has no assignments group: the resolver warns, and
	// the warning reaches the Agent's logger too.
	assert.True(t, freshLogs.Has(slog.LevelWarn, "no assignments for role"),
		"resolver warnings go to the Agent's logger")
}

// tickWithFailedSHA runs one tick of a fresh Agent whose persisted state marks
// the repo's HEAD as failed failures times (recently).
func tickWithFailedSHA(t *testing.T, failures int) (*Agent, *testlog.Recorder) {
	t.Helper()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)
	cfg := &agentcfg.Config{
		Hostname:   "test-host",
		RepoURL:    bareDir,
		RepoBranch: "master",
		SecretsDir: t.TempDir(),
	}
	logger, logs := testlog.New()
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath), WithLogger(logger))

	clone, err := git.PlainClone(filepath.Join(t.TempDir(), "peek"), false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)
	head, err := clone.Head()
	require.NoError(t, err)

	store := state.NewStore(statePath)
	st := state.NewState()
	if failures > 0 {
		st.FailedSHA = head.Hash().String()
		st.FailedCount = failures
		st.FailedAt = time.Now()
	}
	require.NoError(t, store.Save(st))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil, gitpoll.WithLogger(discardLogger()))
	require.NoError(t, poller.Init(ctx))
	require.NoError(t, a.tick(ctx, poller, store, health.New(sys, health.WithLogger(discardLogger()))))
	return a, logs
}

// The credential-expiry series lives in the Agent's store; an expired
// credential warns through the Agent's logger.
func TestPublishCredentialExpiry(t *testing.T) {
	t.Parallel()
	logger, logs := testlog.New()
	a := newTestAgent(t, &agentcfg.Config{Hostname: "test"}, WithLogger(logger))

	a.publishCredentialExpiry("zero", time.Time{})
	a.publishCredentialExpiry("future", time.Unix(1900000000, 0))
	a.publishCredentialExpiry("past", time.Unix(1600000000, 0))

	require.NoError(t, testutil.CollectAndCompare(metrics.NewSecretProviderCollector(a.statusStore), strings.NewReader(`
# HELP picolet_secret_credential_expires_at Unix timestamp at which the configured credential (service-account token, PAT) expires, per provider. Only emitted when the operator records the expiry in config.
# TYPE picolet_secret_credential_expires_at gauge
picolet_secret_credential_expires_at{provider="future"} 1.9e+09
picolet_secret_credential_expires_at{provider="past"} 1.6e+09
`), "picolet_secret_credential_expires_at"))
	assert.True(t, logs.Has(slog.LevelWarn, "secret-provider credential has expired"))
	assert.Equal(t, 1, logs.Count(slog.LevelWarn), "only the past expiry warns")
}
