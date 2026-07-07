package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/status"
)

// newTestRegistry creates a fresh status store + UnitHealthCollector pair
// registered in its own registry, so tests are isolated from each other and
// from the global registry.
func newTestRegistry(t *testing.T) (*status.Store, *prometheus.Registry) {
	t.Helper()
	store := status.NewStore()
	c := metrics.NewUnitHealthCollector(store)
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	return store, reg
}

func TestUnitHealthCollector_Empty(t *testing.T) {
	t.Parallel()
	_, reg := newTestRegistry(t)

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "empty collector should emit no metrics")
}

func TestUnitHealthCollector_ActiveEmitsOne(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)
	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})

	expected := `
# HELP picolet_unit_active 1 if the managed unit is active, 0 if failed. Absent for inactive/oneshot units.
# TYPE picolet_unit_active gauge
picolet_unit_active{unit="foo.service"} 1
# HELP picolet_unit_state_info Info metric (value=1) for managed unit status. Join with picolet_unit_active via group_left.
# TYPE picolet_unit_state_info gauge
picolet_unit_state_info{active_state="active",sub_state="running",unit="foo.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestUnitHealthCollector_FailedEmitsZero(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)
	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "failed", SubState: "auto-restart"})

	expected := `
# HELP picolet_unit_active 1 if the managed unit is active, 0 if failed. Absent for inactive/oneshot units.
# TYPE picolet_unit_active gauge
picolet_unit_active{unit="foo.service"} 0
# HELP picolet_unit_state_info Info metric (value=1) for managed unit status. Join with picolet_unit_active via group_left.
# TYPE picolet_unit_state_info gauge
picolet_unit_state_info{active_state="failed",sub_state="auto-restart",unit="foo.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestUnitHealthCollector_InactiveAbsent(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)
	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "inactive", SubState: "dead"})

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "inactive unit should emit no metrics")
}

func TestUnitHealthCollector_DeleteRemovesMetrics(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)
	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})
	store.DeleteUnit("foo.service")

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "deleted unit should emit no metrics")
}

func TestUnitHealthCollector_StateTransition(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)

	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})
	active, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, active) // unit_active + unit_state_info

	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "failed", SubState: "auto-restart"})
	failed, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, failed)

	store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})
	recovered, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, recovered)
}

func TestUnitHealthCollector_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	store, reg := newTestRegistry(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			store.SetUnit("foo.service", status.UnitRuntimeStatus{ActiveState: "active", SubState: "running"})
			store.DeleteUnit("foo.service")
		}
	}()

	for range 50 {
		_, err := testutil.GatherAndCount(reg)
		require.NoError(t, err)
	}

	<-done
}

func TestUnitDependencyCollector(t *testing.T) {
	t.Parallel()
	store := status.NewStore()
	store.SetDependencies(map[string]status.UnitDependencies{
		"web.service": {
			Requires: []string{"a.service", "b.service"},
			Wants:    []string{"network-online.target"},
			After:    []string{"network-online.target"},
		},
	})

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewUnitDependencyCollector(store)))

	// Zero-count relations (before, binds_to, part_of) are deliberately absent —
	// Prometheus convention is to omit empty series rather than emit zeros.
	expected := `
# HELP picolet_unit_dependency_count Number of generated systemd dependencies for a managed unit by relation.
# TYPE picolet_unit_dependency_count gauge
picolet_unit_dependency_count{relation="after",unit="web.service"} 1
picolet_unit_dependency_count{relation="requires",unit="web.service"} 2
picolet_unit_dependency_count{relation="wants",unit="web.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestUnitDependencyCollector_OmitsZeroRelations(t *testing.T) {
	t.Parallel()
	store := status.NewStore()
	store.SetDependencies(map[string]status.UnitDependencies{
		"only-after.service": {After: []string{"net.service"}},
	})

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewUnitDependencyCollector(store)))

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	// Exactly one series — only "after" has data.
	require.Equal(t, 1, count, "zero-count relations must be absent from scrape output")
}

func TestUnitDependencyCollector_NilStore(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewUnitDependencyCollector(nil)))

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestHostInfoCollector(t *testing.T) {
	t.Parallel()
	store := status.NewStore()
	store.SetHost(status.HostMetadata{PiType: "server", Features: []string{"mqtt", "gpu"}})

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewHostInfoCollector(store)))

	expected := `
# HELP picolet_host_feature_info Resolved host feature metadata (value=1).
# TYPE picolet_host_feature_info gauge
picolet_host_feature_info{feature="gpu"} 1
picolet_host_feature_info{feature="mqtt"} 1
# HELP picolet_host_info Resolved host metadata (value=1).
# TYPE picolet_host_info gauge
picolet_host_info{pi_type="server"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestHostInfoCollector_NilStore(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewHostInfoCollector(nil)))

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func newPruneRegistry(t *testing.T) (*status.Store, *prometheus.Registry) {
	t.Helper()
	store := status.NewStore()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewLastImagePruneCollector(store)))
	return store, reg
}

func TestLastImagePruneCollector_AbsentUntilFirstPrune(t *testing.T) {
	t.Parallel()
	_, reg := newPruneRegistry(t)

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no series should be emitted before the first successful prune")
}

func TestLastImagePruneCollector_EmitsAfterSuccess(t *testing.T) {
	t.Parallel()
	store, reg := newPruneRegistry(t)
	store.SetPrune(status.PruneStatus{LastRunAt: time.Unix(1700000000, 0), ImagesRemoved: 2})

	expected := `
# HELP picolet_last_image_prune_timestamp Unix timestamp of the last successful image prune. Absent until the first prune.
# TYPE picolet_last_image_prune_timestamp gauge
picolet_last_image_prune_timestamp 1.7e+09
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestLastImagePruneCollector_FailureDoesNotEmit(t *testing.T) {
	t.Parallel()
	store, reg := newPruneRegistry(t)
	// A failed attempt records an error (LastErrorAt/Error) but leaves LastRunAt
	// zero, so it must not create the "last successful prune" series — otherwise a
	// failing prune would look healthy.
	store.SetPrune(status.PruneStatus{LastErrorAt: time.Now(), Error: "podman socket unavailable"})

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "a failed prune must not emit the last-success series")
}
