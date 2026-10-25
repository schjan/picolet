package agent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// newPruneTickAgent wires an agent against a minimal real git repo (via
// networkFleet) and a noop-poised state, so tests can exercise the prune step
// through the real tick() path.
func newPruneTickAgent(t *testing.T, opts ...Option) *loopHarness {
	t.Helper()
	metrics.Register(nil)
	bareDir, _ := initFleetRepo(t, networkFleet())
	h := newLoopHarness(t, bareDir, opts...)
	h.cfg.PruneInterval = time.Hour
	h.seedState(t, false, nil) // SHA unchanged => noop tick
	return h
}

// TestTickInvokesPruneOnNoopTick verifies the prune step actually runs through
// the real tick() path on the common no-change tick (placement, not just helper).
func TestTickInvokesPruneOnNoopTick(t *testing.T) {
	t.Parallel()
	pod := newMockPodmanForPruneTick(t)
	pod.EXPECT().ImagePrune(mock.Anything, true).
		Return(applier.PruneResult{ImagesRemoved: 1, ReclaimedBytes: 10}, nil).Once()

	h := newPruneTickAgent(t, WithPodman(pod))

	require.NoError(t, h.tick(t))
	prune := h.a.statusStore.Snapshot().Prune
	assert.False(t, prune.LastRunAt.IsZero(), "prune should have run during the noop tick")
	assert.Equal(t, 1, prune.ImagesRemoved)

	// A subsequent tick must NOT re-run the seed block (ImagePrune is .Once()) and
	// must not clobber the prune counts recorded above.
	require.NoError(t, h.tick(t))
	prune = h.a.statusStore.Snapshot().Prune
	assert.Equal(t, 1, prune.ImagesRemoved, "later ticks must not drop the recorded prune counts")
	assert.Equal(t, uint64(10), prune.ReclaimedBytes)
}

// TestTickSkipsPruneWhenPaused verifies a paused agent does not prune (the step
// is placed after the pause-check early return).
func TestTickSkipsPruneWhenPaused(t *testing.T) {
	t.Parallel()
	// No ImagePrune expectation: the mock fails the test if prune runs.
	pod := newMockPodmanForPruneTick(t)

	h := newPruneTickAgent(t, WithPodman(pod))
	h.a.paused.Store(true)

	require.NoError(t, h.tick(t))
}

// TestTickSeedsPruneStatusFromState verifies the last-prune timestamp survives a
// restart: a non-zero persisted LastPrunedAt seeds the status store on the first
// tick (prune disabled here so the seed is the only thing that can set it).
func TestTickSeedsPruneStatusFromState(t *testing.T) {
	t.Parallel()
	pod := newMockPodmanForPruneTick(t) // disabled prune => never called

	h := newPruneTickAgent(t, WithPodman(pod))
	h.cfg.PruneImages = new(false)

	persisted := time.Now().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	st, err := h.store.Load()
	require.NoError(t, err)
	st.LastPrunedAt = persisted
	require.NoError(t, h.store.Save(st))

	require.NoError(t, h.tick(t))
	got := h.a.statusStore.Snapshot().Prune.LastRunAt
	assert.True(t, persisted.Equal(got), "status store should be seeded from persisted LastPrunedAt: want %s, got %s", persisted, got)
}

func newMockPodmanForPruneTick(t *testing.T) *appliermocks.MockPodmanClient {
	t.Helper()
	pod := appliermocks.NewMockPodmanClient(t)
	// A noop tick may consult managed secrets via the status snapshot refresh;
	// allow it without requiring it.
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	return pod
}

// newPruneState returns a fresh state.Store backed by a temp file, plus a loaded
// state, so prune tests can assert persisted LastPrunedAt.
func newPruneState(t *testing.T) (*state.State, *state.Store) {
	t.Helper()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st := state.NewState()
	require.NoError(t, store.Save(st))
	return st, store
}

func TestMaybePruneImagesRunsWhenDue(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)
	pod.EXPECT().ImagePrune(mock.Anything, true).
		Return(applier.PruneResult{ImagesRemoved: 3, ReclaimedBytes: 4096}, nil).Once()

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)

	a.maybePruneImages(t.Context(), st, store)

	assert.False(t, st.LastPrunedAt.IsZero(), "LastPrunedAt should be advanced")
	loaded, err := store.Load()
	require.NoError(t, err)
	assert.False(t, loaded.LastPrunedAt.IsZero(), "LastPrunedAt should be persisted")

	prune := a.statusStore.Snapshot().Prune
	assert.Equal(t, 3, prune.ImagesRemoved)
	assert.Equal(t, uint64(4096), prune.ReclaimedBytes)
	assert.Empty(t, prune.Error)
}

// TestMaybePruneImagesSkips covers every path that must NOT prune. Each case
// omits the ImagePrune expectation, so the mock fails the test if prune runs.
func TestMaybePruneImagesSkips(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	tests := []struct {
		name string
		cfg  *agentcfg.Config
		opts []Option
		prep func(*state.State)
	}{
		{"not due", &agentcfg.Config{Hostname: "host", PruneInterval: week}, nil, func(st *state.State) { st.LastPrunedAt = time.Now() }},
		{"disabled", &agentcfg.Config{Hostname: "host", PruneImages: new(false), PruneInterval: week}, nil, nil},
		{"dry run", &agentcfg.Config{Hostname: "host", PruneInterval: week}, []Option{WithDryRun(true)}, nil},
		{"zero interval", &agentcfg.Config{Hostname: "host", PruneInterval: 0}, nil, nil},
		{"negative interval", &agentcfg.Config{Hostname: "host", PruneInterval: -time.Hour}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, pod, _ := newBareMocks(t)
			a := newTestAgent(t, tc.cfg, append(tc.opts, WithPodman(pod))...)
			st, store := newPruneState(t)
			if tc.prep != nil {
				tc.prep(st)
			}
			a.maybePruneImages(t.Context(), st, store)
		})
	}
}

func TestMaybePruneImagesFailureKeepsLastPrunedAtAndBacksOff(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)
	// Prune is attempted exactly once: the post-failure cooldown blocks the
	// immediate second call within the same test.
	pod.EXPECT().ImagePrune(mock.Anything, true).
		Return(applier.PruneResult{}, assert.AnError).Once()

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)

	a.maybePruneImages(t.Context(), st, store)
	assert.True(t, st.LastPrunedAt.IsZero(), "LastPrunedAt must not advance on failure")
	prune := a.statusStore.Snapshot().Prune
	assert.Equal(t, assert.AnError.Error(), prune.Error)
	assert.False(t, prune.LastErrorAt.IsZero(), "LastErrorAt should record the failed attempt")
	assert.True(t, prune.LastRunAt.IsZero(), "a failure must not set the last-success timestamp")

	// Second call is gated by the failure cooldown — ImagePrune must NOT run again.
	a.maybePruneImages(t.Context(), st, store)
}

// TestMaybePruneImagesFailurePreservesLastSuccess verifies a failed attempt records
// the error without clobbering the last-success fields the metric reads.
func TestMaybePruneImagesFailurePreservesLastSuccess(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)
	pod.EXPECT().ImagePrune(mock.Anything, true).
		Return(applier.PruneResult{}, assert.AnError).Once()

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)

	// Seed a prior successful prune in the status store.
	lastSuccess := time.Now().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	a.statusStore.SetPrune(status.PruneStatus{LastRunAt: lastSuccess, ImagesRemoved: 5})

	a.maybePruneImages(t.Context(), st, store)

	prune := a.statusStore.Prune()
	assert.Equal(t, lastSuccess, prune.LastRunAt, "failure must preserve last-success timestamp")
	assert.Equal(t, 5, prune.ImagesRemoved, "failure must preserve last-success counts")
	assert.Equal(t, assert.AnError.Error(), prune.Error)
}

// TestMaybePruneImagesPartialFailureCreditsRemovals verifies that when prune
// removes some images but reports an error for others, the removals are still
// reflected in the status snapshot and LastPrunedAt is not advanced (so the
// prune retries).
func TestMaybePruneImagesPartialFailureCreditsRemovals(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)
	pod.EXPECT().ImagePrune(mock.Anything, true).
		Return(applier.PruneResult{ImagesRemoved: 2, ReclaimedBytes: 100}, assert.AnError).Once()

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)

	a.maybePruneImages(t.Context(), st, store)

	assert.True(t, st.LastPrunedAt.IsZero(), "partial failure must not advance LastPrunedAt")
	prune := a.statusStore.Prune()
	assert.Equal(t, 2, prune.ImagesRemoved, "partial removals should be credited to the snapshot")
	assert.Equal(t, uint64(100), prune.ReclaimedBytes)
	assert.Equal(t, assert.AnError.Error(), prune.Error)
}
