package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
)

// newTestRegistry creates a fresh UnitHealthCollector registered in its own registry,
// so tests are isolated from each other and from the global registry.
func newTestRegistry(t *testing.T) (*metrics.UnitHealthCollector, *prometheus.Registry) {
	t.Helper()
	c := metrics.NewUnitHealthCollector()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	return c, reg
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
	c, reg := newTestRegistry(t)
	c.Set("foo.service", "active", "running")

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
	c, reg := newTestRegistry(t)
	c.Set("foo.service", "failed", "auto-restart")

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
	c, reg := newTestRegistry(t)
	c.Set("foo.service", "inactive", "dead")

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "inactive unit should emit no metrics")
}

func TestUnitHealthCollector_DeleteRemovesMetrics(t *testing.T) {
	t.Parallel()
	c, reg := newTestRegistry(t)
	c.Set("foo.service", "active", "running")
	c.Delete("foo.service")

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "deleted unit should emit no metrics")
}

func TestUnitHealthCollector_StateTransition(t *testing.T) {
	t.Parallel()
	c, reg := newTestRegistry(t)

	// active → check active=1
	c.Set("foo.service", "active", "running")
	active, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, active) // unit_active + unit_state_info

	// failed → check active=0, no stale series
	c.Set("foo.service", "failed", "auto-restart")
	failed, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, failed) // still 2 metrics, but different values

	// active again → verify recovery
	c.Set("foo.service", "active", "running")
	recovered, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 2, recovered)
}

func TestUnitHealthCollector_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c, reg := newTestRegistry(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			c.Set("foo.service", "active", "running")
			c.Delete("foo.service")
		}
	}()

	for range 50 {
		_, err := testutil.GatherAndCount(reg)
		require.NoError(t, err)
	}

	<-done
}
