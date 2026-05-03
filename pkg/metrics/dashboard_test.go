package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/status"
)

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
