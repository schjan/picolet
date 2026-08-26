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

// newRunRegistry creates a fresh status store + UnitRunCollector pair registered
// in its own registry, isolated from other tests and from the global registry.
func newRunRegistry(t *testing.T) (*status.Store, *prometheus.Registry) {
	t.Helper()
	store := status.NewStore()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewUnitRunCollector(store)))
	return store, reg
}

const (
	runHelp = `
# HELP picolet_unit_last_run_timestamp_seconds Unix timestamp at which a timer-triggered one-shot last started, whatever the outcome. Absent until the first run.
# TYPE picolet_unit_last_run_timestamp_seconds gauge
`
	successHelp = `
# HELP picolet_unit_last_success_timestamp_seconds Unix timestamp at which a timer-triggered one-shot last completed successfully. Absent until the first success.
# TYPE picolet_unit_last_success_timestamp_seconds gauge
`
	resultHelp = `
# HELP picolet_unit_last_result Info metric (value=1) for a timer-triggered one-shot's current systemd Result= (success, exit-code, timeout, signal, ...). systemd resets it to success when a run starts, so join with picolet_unit_last_success_timestamp_seconds when the last completed outcome matters. Absent until the unit has run.
# TYPE picolet_unit_last_result gauge
`
	triggerHelp = `
# HELP picolet_timer_last_trigger_timestamp_seconds Unix timestamp at which a managed .timer last fired. Absent until the first trigger.
# TYPE picolet_timer_last_trigger_timestamp_seconds gauge
`
)

func TestUnitRunCollector_Empty(t *testing.T) {
	t.Parallel()
	_, reg := newRunRegistry(t)

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "an empty store emits no run series")
}

func TestUnitRunCollector_NilStore(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewUnitRunCollector(nil)))

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestUnitRunCollector_Success(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	started := time.Unix(1_800_000_000, 0).UTC()
	finished := started.Add(30 * time.Second)
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: started, FinishedAt: finished, Result: "success",
	})

	expected := runHelp + `picolet_unit_last_run_timestamp_seconds{unit="backup.service"} 1.8e+09
` + successHelp + `picolet_unit_last_success_timestamp_seconds{unit="backup.service"} 1.80000003e+09
` + resultHelp + `picolet_unit_last_result{result="success",unit="backup.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestUnitRunCollector_Failure(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	started := time.Unix(1_800_000_000, 0).UTC()
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: started, FinishedAt: started.Add(time.Second), Result: "exit-code",
	})

	// No last-success series at all: the job has never been seen to succeed, so
	// `absent()` reports it instead of an epoch-old timestamp.
	expected := runHelp + `picolet_unit_last_run_timestamp_seconds{unit="backup.service"} 1.8e+09
` + resultHelp + `picolet_unit_last_result{result="exit-code",unit="backup.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

// systemd reports Result="success" for a unit that has never run, so a never-run
// job must still export nothing — including no result series.
func TestUnitRunCollector_NeverRanEmitsNothing(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	store.ObserveRun("backup.service", status.RunObservation{Result: "success"})
	store.ObserveRun("backup.timer", status.RunObservation{})

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "a tracked unit that has never run must export nothing")
}

// While a run is in flight the result series follows systemd — which has already
// reset Result= to "success" — but last-success stays on the last run actually
// observed to succeed, so the staleness alert does not clear just because a new
// attempt started.
func TestUnitRunCollector_RunningKeepsLastSuccess(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	firstStart := time.Unix(1_800_000_000, 0).UTC()
	firstEnd := firstStart.Add(time.Second)
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: firstStart, FinishedAt: firstEnd, Result: "success",
	})
	secondStart := firstStart.Add(time.Hour)
	secondEnd := secondStart.Add(time.Second)
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: secondStart, FinishedAt: secondEnd, Result: "exit-code",
	})
	// Third run in flight: start moved, finish did not, Result reads "success".
	thirdStart := secondStart.Add(time.Hour)
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: thirdStart, FinishedAt: secondEnd, Result: "success",
	})

	expected := runHelp + `picolet_unit_last_run_timestamp_seconds{unit="backup.service"} 1.8000072e+09
` + successHelp + `picolet_unit_last_success_timestamp_seconds{unit="backup.service"} 1.800000001e+09
` + resultHelp + `picolet_unit_last_result{result="success",unit="backup.service"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestUnitRunCollector_TimerTrigger(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	store.ObserveRun("backup.timer", status.RunObservation{
		TriggeredAt: time.Unix(1_800_000_000, 0).UTC(),
	})

	expected := triggerHelp + `picolet_timer_last_trigger_timestamp_seconds{unit="backup.timer"} 1.8e+09
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

// Only a unit leaving the Fleet drops its series; a failed health pass does not.
func TestUnitRunCollector_PrunedUnitDisappears(t *testing.T) {
	t.Parallel()
	store, reg := newRunRegistry(t)
	started := time.Unix(1_800_000_000, 0).UTC()
	store.ObserveRun("backup.service", status.RunObservation{
		StartedAt: started, FinishedAt: started.Add(time.Second), Result: "success",
	})

	store.ClearUnits()
	after, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 3, after, "an all-failed health pass must not drop run series")

	store.PruneRuns(nil)
	pruned, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)
}
