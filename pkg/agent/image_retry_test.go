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
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/state"
)

// A .image is report-only for the health loop, but the pull its apply-time
// restart starts is picolet's own: when that pull fails, the pending record
// must survive the next health pass (so the tick reports retry_pending, not a
// clean no-op) and the pull must be retried once the cooldown has passed.
func TestFailedImagePullAtApplyIsRetriedByHealth(t *testing.T) {
	t.Parallel()
	const unit = "redis-image.service"
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", "images: {}\nports: {}\n")
	writeTestFile(t, repoDir, "assignments.yml", "base:\n  images:\n    - quadlets/images/redis.image\n")
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", "hostname: test-host\nrole: server\nfeatures: []\n")
	writeTestFile(t, repoDir, "quadlets/images/redis.image", "[Image]\nImage=docker.io/library/redis:7\n")

	sys, pod, fw := newBareMocks(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, unit).Return(assert.AnError).Once()
	sys.EXPECT().GetUnitStatus(mock.Anything, unit).Return(applier.UnitStatus{
		ActiveState: "failed", SubState: "failed", UnitFileState: "generated", ServiceType: "oneshot",
	}, nil)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()},
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw), WithRepoPath(repoDir))
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, store.Save(state.NewState()))

	_, err := a.ReconcileOnce(t.Context(), "head-sha", state.NewState(), store)
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
	st, err := store.Load()
	require.NoError(t, err)
	require.Contains(t, st.PendingUnits, unit)

	// Next pass, within the cooldown: not retried yet, and still pending.
	checker := health.New(sys)
	result, err := checker.Enforce(t.Context(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{unit}, result.Skipped)
	assert.Empty(t, result.ExternallyActivated)
	require.Contains(t, st.PendingUnits, unit, "the tick must stay retry_pending, not become a clean no-op")

	// Once the cooldown has passed, the pull is retried. Time is simulated by
	// aging the record; a fresh checker has no in-memory cooldown to outlast.
	pu := st.PendingUnits[unit]
	pu.LastAttemptAt = time.Now().Add(-time.Hour)
	st.PendingUnits[unit] = pu
	sys.EXPECT().RestartUnit(mock.Anything, unit).Return(nil).Once()
	result, err = health.New(sys).Enforce(t.Context(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{unit}, result.Restarted)
}
