package agent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

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

func TestMaybePruneImagesSkipsWhenNotDue(t *testing.T) {
	t.Parallel()
	// No ImagePrune expectation: the mock fails the test if it is called.
	_, pod, _ := newBareMocks(t)

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)
	st.LastPrunedAt = time.Now() // just pruned

	a.maybePruneImages(t.Context(), st, store)
}

func TestMaybePruneImagesSkipsWhenDisabled(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)

	cfg := &agentcfg.Config{Hostname: "host", PruneImages: new(false), PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod))
	st, store := newPruneState(t)

	a.maybePruneImages(t.Context(), st, store)
}

func TestMaybePruneImagesSkipsInDryRun(t *testing.T) {
	t.Parallel()
	_, pod, _ := newBareMocks(t)

	cfg := &agentcfg.Config{Hostname: "host", PruneInterval: 7 * 24 * time.Hour}
	a := newTestAgent(t, cfg, WithPodman(pod), WithDryRun(true))
	st, store := newPruneState(t)

	a.maybePruneImages(t.Context(), st, store)
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
	assert.Equal(t, assert.AnError.Error(), a.statusStore.Snapshot().Prune.Error)

	// Second call is gated by the failure cooldown — ImagePrune must NOT run again.
	a.maybePruneImages(t.Context(), st, store)
}
