package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/status"
)

func TestRecordHealthMetrics_ClearsStaleGauges(t *testing.T) {
	t.Parallel()

	// D-Bus fully down: all errors, no statuses.
	a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

	// Seed a unit into the agent's status store (the metrics collector reads from it).
	a.statusStore.SetUnit("clear-test.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})

	a.recordHealthMetrics(&health.CheckResult{
		Errors:   []error{fmt.Errorf("dbus dead")},
		Statuses: map[string]applier.UnitStatus{},
	})

	// Status store is the single source of truth — collector scrapes from it.
	assert.Empty(t, a.statusStore.Snapshot().Units, "store should clear stale unit state when D-Bus is down")

	collector := metrics.NewUnitHealthCollector(a.statusStore)
	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)
	count := 0
	for range ch {
		count++
	}
	assert.Equal(t, 0, count, "cleared collector should emit no metrics")
}

func TestRecordHealthMetrics_UpdatesStatusStore(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)
	a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

	a.recordHealthMetrics(&health.CheckResult{
		Statuses: map[string]applier.UnitStatus{
			"web.service": {ActiveState: "active", SubState: "running"},
		},
	})

	snap := a.statusStore.Snapshot()
	assert.Equal(t, "active", snap.Units["web.service"].ActiveState)
	assert.Equal(t, "running", snap.Units["web.service"].SubState)
}

// A per-unit query failure and a wholesale D-Bus outage both leave the job's
// last-known run record intact — the health pass reports the Fleet's whole unit
// set, errored units included, so nothing is pruned. Only a unit that has left
// the Fleet loses its record.
func TestRecordHealthMetrics_RunRecordLifecycle(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)
	a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

	started := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	a.recordHealthMetrics(&health.CheckResult{
		Statuses: map[string]applier.UnitStatus{
			"job.service": {
				ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot",
				TriggeredBy:       []string{"job.timer"},
				LastRunStartedAt:  started,
				LastRunFinishedAt: finished,
				Result:            "success",
			},
		},
		TimerJobs: []string{"job.service"},
		Managed:   []string{"job.service"},
	})
	require.Equal(t, finished, a.statusStore.Snapshot().Runs["job.service"].SucceededAt)

	// The unit's query failed this pass: no status, no classification, still managed.
	a.recordHealthMetrics(&health.CheckResult{
		Statuses: map[string]applier.UnitStatus{},
		Errors:   []error{fmt.Errorf("dbus timeout")},
		Managed:  []string{"job.service"},
	})
	snap := a.statusStore.Snapshot()
	assert.Empty(t, snap.Units, "an all-failed pass clears live unit state")
	assert.Equal(t, finished, snap.Runs["job.service"].SucceededAt,
		"a failed pass must not drop the last-success value")

	// The unit left the Fleet: its record goes with it.
	a.recordHealthMetrics(&health.CheckResult{Statuses: map[string]applier.UnitStatus{}})
	assert.NotContains(t, a.statusStore.Snapshot().Runs, "job.service")
}

func TestSetFilesManagedMetric(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)

	counts := map[string]float64{
		"container": 3,
		"network":   1,
		"volume":    0,
		"kube":      2,
		"systemd":   0,
		"manifest":  0,
		"secret":    5,
	}
	setFilesManagedMetric(counts)

	for _, cat := range reconciler.Categories() {
		category := cat.String()
		got := testutil.ToFloat64(metrics.FilesManagedTotal.WithLabelValues(category))
		assert.InDelta(t, counts[category], got, 0.001, "category %s", category)
	}
}

func TestPublishCredentialExpiry(t *testing.T) {
	t.Parallel()

	provider := resolver.ProviderKey("test-provider-" + t.Name()) // unique label so parallel tests don't clobber each other
	expiry := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	// Zero value: gauge is not touched, no series emitted.
	publishCredentialExpiry(provider+"-zero", time.Time{})
	zero, err := metrics.SecretCredentialExpiresAt.GetMetricWithLabelValues(string(provider) + "-zero")
	require.NoError(t, err)
	assert.InDelta(t, 0.0, testutil.ToFloat64(zero), 0, "no value should be set for zero time.Time")

	// Future value: gauge holds the Unix timestamp.
	publishCredentialExpiry(provider, expiry)
	got, err := metrics.SecretCredentialExpiresAt.GetMetricWithLabelValues(string(provider))
	require.NoError(t, err)
	assert.InDelta(t, float64(expiry.Unix()), testutil.ToFloat64(got), 0)

	// Past value: still published (alerts need to see it), no error.
	pastProvider := provider + "-past"
	past := time.Now().Add(-24 * time.Hour)
	publishCredentialExpiry(pastProvider, past)
	gotPast, err := metrics.SecretCredentialExpiresAt.GetMetricWithLabelValues(string(pastProvider))
	require.NoError(t, err)
	assert.InDelta(t, float64(past.Unix()), testutil.ToFloat64(gotPast), 0)
}
