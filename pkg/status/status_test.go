package status

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreSnapshotDeepCopy(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.SetUnits(map[string]UnitRuntimeStatus{
		"web.service": {ActiveState: "active", SubState: "running"},
	})
	store.SetDependencies(map[string]UnitDependencies{
		"web.service": {Requires: []string{"network.service"}},
	})
	store.SetHost(HostMetadata{Role: "server", Features: []string{"mqtt"}})

	snap := store.Snapshot()
	snap.Units["web.service"] = UnitRuntimeStatus{ActiveState: "failed"}
	snap.Dependencies["web.service"].Requires[0] = "mutated.service"
	snap.Host.Features[0] = "mutated"

	fresh := store.Snapshot()
	assert.Equal(t, "active", fresh.Units["web.service"].ActiveState)
	assert.Equal(t, []string{"network.service"}, fresh.Dependencies["web.service"].Requires)
	assert.Equal(t, []string{"mqtt"}, fresh.Host.Features)
}

func TestStoreEventsTrimToLimit(t *testing.T) {
	t.Parallel()
	store := NewStore()
	now := time.Now()
	for i := range 55 {
		store.AddEvent(ReconcileEvent{
			At:      now.Add(time.Duration(i) * time.Second),
			Result:  "noop",
			Message: fmt.Sprintf("event-%02d", i),
		})
	}

	events := store.Snapshot().Events
	assert.Len(t, events, 50)
	assert.Equal(t, "event-05", events[0].Message)
	assert.Equal(t, "event-54", events[49].Message)
}

func TestUnitDependenciesIsEmpty(t *testing.T) {
	t.Parallel()
	assert.True(t, UnitDependencies{}.IsEmpty())
	assert.False(t, UnitDependencies{Requires: []string{"x"}}.IsEmpty())
	assert.False(t, UnitDependencies{PartOf: []string{"x"}}.IsEmpty())
}

func TestStoreSetUnit(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.SetUnit("web.service", UnitRuntimeStatus{ActiveState: "active", SubState: "running"})
	store.SetUnit("db.service", UnitRuntimeStatus{ActiveState: "failed", SubState: "dead"})

	snap := store.Snapshot()
	assert.Equal(t, "active", snap.Units["web.service"].ActiveState)
	assert.Equal(t, "failed", snap.Units["db.service"].ActiveState)

	store.DeleteUnit("web.service")
	snap = store.Snapshot()
	_, ok := snap.Units["web.service"]
	assert.False(t, ok)
	assert.Equal(t, "failed", snap.Units["db.service"].ActiveState)
}

func TestStoreBootstrappedFlipsOnFirstSetDependencies(t *testing.T) {
	t.Parallel()
	store := NewStore()
	assert.False(t, store.Snapshot().Bootstrapped, "fresh store is not bootstrapped")

	store.SetHost(HostMetadata{Role: "server"})
	assert.False(t, store.Snapshot().Bootstrapped, "SetHost alone does not bootstrap")

	store.SetDependencies(nil)
	assert.True(t, store.Snapshot().Bootstrapped, "SetDependencies (even nil) bootstraps")
}

func TestStoreBootstrappedRaceSafe(t *testing.T) {
	t.Parallel()
	store := NewStore()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			store.SetDependencies(map[string]UnitDependencies{
				"web.service": {Requires: []string{"net.service"}},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_ = store.Snapshot().Bootstrapped
		}
	}()
	wg.Wait()
	assert.True(t, store.Snapshot().Bootstrapped)
}

// A one-shot's run history is built from successive observations of the same
// systemd properties: the last-success timestamp is derived here, not read from
// systemd, because systemd resets Result= to "success" when a run starts.
func TestStoreObserveRunDerivesLastSuccess(t *testing.T) {
	t.Parallel()
	store := NewStore()

	firstStart := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	firstEnd := firstStart.Add(time.Minute)
	store.ObserveRun("backup.service", RunObservation{
		StartedAt: firstStart, FinishedAt: firstEnd, Result: "success",
	})

	run := store.Snapshot().Runs["backup.service"]
	assert.Equal(t, firstStart, run.StartedAt)
	assert.Equal(t, firstEnd, run.FinishedAt)
	assert.Equal(t, firstEnd, run.SucceededAt)
	assert.Equal(t, "success", run.Result)

	// An unchanged observation (the job is simply idle) must not move anything.
	store.ObserveRun("backup.service", RunObservation{
		StartedAt: firstStart, FinishedAt: firstEnd, Result: "success",
	})
	assert.Equal(t, firstEnd, store.Snapshot().Runs["backup.service"].SucceededAt)

	// A later run that fails advances the run/result but never last-success.
	secondStart := firstStart.Add(time.Hour)
	secondEnd := secondStart.Add(time.Minute)
	store.ObserveRun("backup.service", RunObservation{
		StartedAt: secondStart, FinishedAt: secondEnd, Result: "exit-code",
	})

	run = store.Snapshot().Runs["backup.service"]
	assert.Equal(t, secondStart, run.StartedAt)
	assert.Equal(t, "exit-code", run.Result)
	assert.Equal(t, firstEnd, run.SucceededAt, "a failed run must not advance last success")
}

// While a run is in flight systemd has already reset Result= to "success" and has
// not yet moved InactiveEnterTimestamp. The result series follows systemd (that is
// the unit's current result), but last-success must not advance, or every failing
// job would look like it had succeeded for the duration of its next attempt.
func TestStoreObserveRunInFlightKeepsLastSuccess(t *testing.T) {
	t.Parallel()
	store := NewStore()

	start := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	store.ObserveRun("verify.service", RunObservation{StartedAt: start, FinishedAt: end, Result: "success"})
	failStart := start.Add(time.Hour)
	failEnd := failStart.Add(time.Minute)
	store.ObserveRun("verify.service", RunObservation{StartedAt: failStart, FinishedAt: failEnd, Result: "timeout"})
	require.Equal(t, end, store.Snapshot().Runs["verify.service"].SucceededAt)

	// Third run starts: InactiveExitTimestamp moves, InactiveEnterTimestamp does
	// not, Result reads "success" again.
	runningStart := failStart.Add(time.Hour)
	store.ObserveRun("verify.service", RunObservation{StartedAt: runningStart, FinishedAt: failEnd, Result: "success"})

	run := store.Snapshot().Runs["verify.service"]
	assert.Equal(t, runningStart, run.StartedAt, "the in-flight run is the last run")
	assert.Equal(t, "success", run.Result, "the result series reports the unit's current result")
	assert.Equal(t, end, run.SucceededAt, "an in-flight run must not advance last success")
}

// A unit that has never run yields a record with no timestamps and no result, so
// the collector can omit its series entirely instead of exporting zeros.
func TestStoreObserveRunNeverRan(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.ObserveRun("fresh.service", RunObservation{})

	run, ok := store.Snapshot().Runs["fresh.service"]
	assert.True(t, ok, "an observed unit is tracked even before its first run")
	assert.Zero(t, run.StartedAt)
	assert.Zero(t, run.SucceededAt)
	assert.Empty(t, run.Result)
}

// Run records have their own lifecycle: unlike Units they survive a failed health
// pass, so a D-Bus hiccup never makes the last-success series flap.
func TestStoreRunsSurviveClearUnits(t *testing.T) {
	t.Parallel()
	store := NewStore()
	start := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	store.SetUnit("backup.service", UnitRuntimeStatus{ActiveState: "inactive", SubState: "dead"})
	store.ObserveRun("backup.service", RunObservation{StartedAt: start, FinishedAt: start, Result: "success"})

	store.ClearUnits()

	snap := store.Snapshot()
	assert.Empty(t, snap.Units)
	assert.Equal(t, start, snap.Runs["backup.service"].SucceededAt)
}

func TestStorePruneRuns(t *testing.T) {
	t.Parallel()
	store := NewStore()
	started := time.Date(2026, 8, 20, 4, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	store.ObserveRun("kept.service", RunObservation{StartedAt: started, FinishedAt: finished, Result: "success"})
	store.ObserveRun("kept.timer", RunObservation{TriggeredAt: finished})
	store.ObserveRun("gone.service", RunObservation{StartedAt: started, FinishedAt: finished, Result: "success"})

	store.PruneRuns([]string{"kept.service", "kept.timer"})

	runs := store.Snapshot().Runs
	assert.Contains(t, runs, "kept.service")
	assert.Contains(t, runs, "kept.timer")
	assert.NotContains(t, runs, "gone.service", "a unit that left the fleet must lose its record")
}

// A first run still in flight reports its current result but credits no success:
// systemd has already set Result=success for a run that has not finished.
func TestStoreObserveRunFirstRunInFlight(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.ObserveRun("first.service", RunObservation{
		StartedAt: time.Date(2026, 8, 20, 5, 0, 0, 0, time.UTC), Result: "success",
	})

	run := store.Snapshot().Runs["first.service"]
	assert.Equal(t, "success", run.Result)
	assert.Zero(t, run.SucceededAt, "a run that has not finished cannot be a last success")
}

// A fresh store observing a unit whose next run is already in flight must credit
// no success: systemd reports the *previous* run's finish timestamp together with
// a Result= already reset to "success", so trusting it would credit a success to a
// run that may well have failed.
func TestStoreObserveRunInFlightOnFirstObservation(t *testing.T) {
	t.Parallel()
	store := NewStore()
	lastFinish := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	store.ObserveRun("verify.service", RunObservation{
		StartedAt:  lastFinish.Add(time.Hour), // a run started after the last finish
		FinishedAt: lastFinish,
		Result:     "success",
	})

	run := store.Snapshot().Runs["verify.service"]
	assert.Equal(t, lastFinish.Add(time.Hour), run.StartedAt, "the in-flight run is still the last run")
	assert.Zero(t, run.SucceededAt, "no success may be credited to an unobserved run")
}

func TestStoreSnapshotRunsDeepCopy(t *testing.T) {
	t.Parallel()
	store := NewStore()
	started := time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
	store.ObserveRun("backup.service", RunObservation{
		StartedAt: started, FinishedAt: started.Add(time.Minute), Result: "success",
	})

	snap := store.Snapshot()
	snap.Runs["backup.service"] = UnitRun{Result: "mutated"}

	assert.Equal(t, "success", store.Snapshot().Runs["backup.service"].Result)
}
