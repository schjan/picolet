package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// failingApplyAgent returns a harness over a fresh test repo whose every
// file write fails, so each reconciliation of HEAD fails at apply, plus the
// repo's HEAD SHA.
func failingApplyAgent(t *testing.T, pollInterval time.Duration) (h *loopHarness, headSHA string) {
	t.Helper()
	bareDir, headSHA := initFleetRepo(t, networkFleet())
	h = newLoopHarness(t, bareDir)
	h.cfg.PollInterval = pollInterval
	h.writeErr = fmt.Errorf("simulated disk error")
	return h, headSHA
}

func eventsWithResult(snap status.Snapshot, result string) []status.ReconcileEvent {
	var out []status.ReconcileEvent
	for _, e := range snap.Events {
		if e.Result == result {
			out = append(out, e)
		}
	}
	return out
}

// A credential fix arrives with an Agent restart but leaves the SHA unchanged:
// the restart clears the gate, so the first tick retries the gated SHA.
func TestAgentRestartClearsFailureGate(t *testing.T) {
	t.Parallel()
	h, headSHA := failingApplyAgent(t, time.Hour)

	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 3       // gate limit reached before the restart
	st.FailedAt = time.Now() // and not yet expired
	require.NoError(t, h.store.Save(st))

	h.runUntil(t, func() bool { return len(eventsWithResult(h.a.statusStore.Snapshot(), "failure")) > 0 })

	snap := h.a.statusStore.Snapshot()
	cleared := eventsWithResult(snap, "failed_sha_gate_cleared")
	require.Len(t, cleared, 1, "clearing a non-zero gate records exactly one event")
	assert.Equal(t, headSHA, cleared[0].SHA)
	assert.Contains(t, cleared[0].Message, "restart")
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate"), "the first tick is not skipped")

	// The first tick retried and failed again: the count restarted from zero.
	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, headSHA, loaded.FailedSHA)
	assert.Equal(t, 1, loaded.FailedCount)
	assert.InDelta(t, 1.0, testutil.ToFloat64(metrics.NewFailedSHACollector(h.a.statusStore)), 0)
}

// Clearing at startup resets the gauge before any tick runs; a zero gate
// records no event and leaves state alone.
func TestClearFailureGateOnStartup(t *testing.T) {
	t.Parallel()
	h, headSHA := failingApplyAgent(t, time.Hour)

	h.a.clearFailureGateOnStartup(h.store)
	assert.Empty(t, h.a.statusStore.Snapshot().Events, "a zero gate records nothing")

	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 3
	st.FailedAt = time.Now()
	require.NoError(t, h.store.Save(st))
	h.a.statusStore.SetFailedSHACount(3)

	h.a.clearFailureGateOnStartup(h.store)

	assert.InDelta(t, 0.0, testutil.ToFloat64(metrics.NewFailedSHACollector(h.a.statusStore)), 0)
	events := h.a.statusStore.Snapshot().Events
	require.Len(t, events, 1)
	assert.Equal(t, headSHA, events[0].SHA)
	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Zero(t, loaded.FailedCount)
	assert.Empty(t, loaded.FailedSHA)
}

// Within one run, a gate older than its expiry no longer skips the SHA.
func TestTickRetriesExpiredFailureGate(t *testing.T) {
	t.Parallel()
	h, headSHA := failingApplyAgent(t, time.Hour)
	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 5                           // well past the limit
	st.FailedAt = time.Now().Add(-2 * time.Hour) // but older than the 1h expiry
	require.NoError(t, h.store.Save(st))

	require.Error(t, h.tick(t),
		"the expired gate lets the reconciliation run (and fail again)")

	snap := h.a.statusStore.Snapshot()
	assert.Len(t, eventsWithResult(snap, "failure"), 1)
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate"))
}

// Within one Agent run the gate still holds: after three failures of the same
// SHA, later ticks skip it.
func TestAgentGatesSHAAfterThreeFailuresInOneRun(t *testing.T) {
	t.Parallel()
	h, headSHA := failingApplyAgent(t, 50*time.Millisecond)

	h.runUntil(t, func() bool { return len(eventsWithResult(h.a.statusStore.Snapshot(), "failed_sha_gate")) > 0 })

	snap := h.a.statusStore.Snapshot()
	assert.Len(t, eventsWithResult(snap, "failure"), 3)
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate_cleared"), "a fresh state has no gate to clear")
	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, headSHA, loaded.FailedSHA)
	assert.Equal(t, 3, loaded.FailedCount)
	assert.InDelta(t, 3.0, testutil.ToFloat64(metrics.NewFailedSHACollector(h.a.statusStore)), 0)
}
