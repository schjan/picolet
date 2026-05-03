package status

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	store.SetHost(HostMetadata{PiType: "server", Features: []string{"mqtt"}})

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

	store.SetHost(HostMetadata{PiType: "server"})
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
