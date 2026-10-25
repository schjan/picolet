package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// failingApplyAgent returns an Agent over a fresh test repo whose every file
// write fails, so each reconciliation of HEAD fails at apply, plus the repo's
// HEAD SHA and the state path.
func failingApplyAgent(t *testing.T, pollInterval time.Duration) (a *Agent, headSHA, statePath string) {
	t.Helper()
	bareDir, headSHA := initFleetRepo(t, networkFleet())
	statePath = filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	setupNoopMocks(sys, pod)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(fmt.Errorf("simulated disk error")).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: pollInterval,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
	a = newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(filepath.Join(t.TempDir(), "clone")), WithStatePath(statePath))

	return a, headSHA, statePath
}

// runUntil runs the Agent until cond holds on its status store (or fails the
// test after a timeout), then stops it.
func runUntil(t *testing.T, a *Agent, cond func(status.Snapshot) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	assert.Eventually(t, func() bool { return cond(a.statusStore.Snapshot()) }, 8*time.Second, 20*time.Millisecond)
	cancel()
	require.NoError(t, <-errCh)
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
	a, headSHA, statePath := failingApplyAgent(t, time.Hour)

	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 3       // gate limit reached before the restart
	st.FailedAt = time.Now() // and not yet expired
	require.NoError(t, state.NewStore(statePath).Save(st))

	runUntil(t, a, func(s status.Snapshot) bool { return len(eventsWithResult(s, "failure")) > 0 })

	snap := a.statusStore.Snapshot()
	cleared := eventsWithResult(snap, "failed_sha_gate_cleared")
	require.Len(t, cleared, 1, "clearing a non-zero gate records exactly one event")
	assert.Equal(t, headSHA, cleared[0].SHA)
	assert.Contains(t, cleared[0].Message, "restart")
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate"), "the first tick is not skipped")

	// The first tick retried and failed again: the count restarted from zero.
	loaded, err := state.NewStore(statePath).Load()
	require.NoError(t, err)
	assert.Equal(t, headSHA, loaded.FailedSHA)
	assert.Equal(t, 1, loaded.FailedCount)
	assert.InDelta(t, 1.0, testutil.ToFloat64(metrics.NewFailedSHACollector(a.statusStore)), 0)
}

// Clearing at startup resets the gauge before any tick runs; a zero gate
// records no event and leaves state alone.
func TestClearFailureGateOnStartup(t *testing.T) {
	t.Parallel()
	a, headSHA, statePath := failingApplyAgent(t, time.Hour)
	store := state.NewStore(statePath)

	a.clearFailureGateOnStartup(store)
	assert.Empty(t, a.statusStore.Snapshot().Events, "a zero gate records nothing")

	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 3
	st.FailedAt = time.Now()
	require.NoError(t, store.Save(st))
	a.statusStore.SetFailedSHACount(3)

	a.clearFailureGateOnStartup(store)

	assert.InDelta(t, 0.0, testutil.ToFloat64(metrics.NewFailedSHACollector(a.statusStore)), 0)
	events := a.statusStore.Snapshot().Events
	require.Len(t, events, 1)
	assert.Equal(t, headSHA, events[0].SHA)
	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Zero(t, loaded.FailedCount)
	assert.Empty(t, loaded.FailedSHA)
}

// Within one run, a gate older than its expiry no longer skips the SHA.
func TestTickRetriesExpiredFailureGate(t *testing.T) {
	t.Parallel()
	a, headSHA, statePath := failingApplyAgent(t, time.Hour)
	store := state.NewStore(statePath)
	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 5                           // well past the limit
	st.FailedAt = time.Now().Add(-2 * time.Hour) // but older than the 1h expiry
	require.NoError(t, store.Save(st))

	ctx := t.Context()
	poller := gitpoll.New(a.cfg.RepoURL, a.cfg.RepoBranch, a.repoPath, nil, gitpoll.WithLogger(discardLogger()))
	require.NoError(t, poller.Init(ctx))
	require.Error(t, a.tick(ctx, poller, store, health.New(a.systemd, health.WithLogger(discardLogger()))),
		"the expired gate lets the reconciliation run (and fail again)")

	snap := a.statusStore.Snapshot()
	assert.Len(t, eventsWithResult(snap, "failure"), 1)
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate"))
}

// Within one Agent run the gate still holds: after three failures of the same
// SHA, later ticks skip it.
func TestAgentGatesSHAAfterThreeFailuresInOneRun(t *testing.T) {
	t.Parallel()
	a, headSHA, statePath := failingApplyAgent(t, 50*time.Millisecond)

	runUntil(t, a, func(s status.Snapshot) bool { return len(eventsWithResult(s, "failed_sha_gate")) > 0 })

	snap := a.statusStore.Snapshot()
	assert.Len(t, eventsWithResult(snap, "failure"), 3)
	assert.Empty(t, eventsWithResult(snap, "failed_sha_gate_cleared"), "a fresh state has no gate to clear")
	loaded, err := state.NewStore(statePath).Load()
	require.NoError(t, err)
	assert.Equal(t, headSHA, loaded.FailedSHA)
	assert.Equal(t, 3, loaded.FailedCount)
	assert.InDelta(t, 3.0, testutil.ToFloat64(metrics.NewFailedSHACollector(a.statusStore)), 0)
}
