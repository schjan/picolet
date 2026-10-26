package agent

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/pkg/agentcfg"
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

	n := a1.recordProviderRefCount(resolver.ProviderOnePassword, true, isRef, refFiles)
	a1.recordProviderSyncSuccess(resolver.ProviderOnePassword, n)
	a1.recordProviderSyncSuccess(resolver.ProviderOnePassword, n)
	a2.recordProviderRefCount(resolver.ProviderOnePassword, true, isRef, refFiles[2:])

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
	bareDir, headSHA := initFleetRepo(t, networkFleet())
	logger, logs := testlog.New()
	h := newLoopHarness(t, bareDir, WithLogger(logger))

	st := state.NewState()
	if failures > 0 {
		st.FailedSHA = headSHA
		st.FailedCount = failures
		st.FailedAt = time.Now()
	}
	require.NoError(t, h.store.Save(st))
	require.NoError(t, h.tick(t))
	return h.a, logs
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
