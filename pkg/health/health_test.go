package health

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

func TestEnforceAllHealthy(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.MatchedBy(func(s string) bool {
		return s == "foo.service" || s == "bar-network.service"
	})).Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Times(2)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
			"/etc/containers/systemd/bar.network":   {Hash: "sha256:def", Category: "network"},
			"secret:my_secret":                      {Hash: "sha256:ghi", Category: "secret"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
			"/etc/containers/systemd/bar.network":   "bar-network.service",
			// secret has no entry — skipped automatically
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Healthy, 2)
	assert.Empty(t, result.Unhealthy)
	assert.Len(t, result.Statuses, 2)
}

func TestEnforceRestartsUnhealthy(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "auto-restart"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Unhealthy, 1)
	assert.Equal(t, []string{"foo.service"}, result.Restarted)
	assert.Empty(t, result.Skipped)
}

func TestEnforceOneshotExitedIsHealthy(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "exited"}, nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Contains(t, result.Healthy, "foo.service")
	assert.Empty(t, result.Unhealthy)
	assert.Empty(t, result.Restarted)
}

func TestEnforceInactiveSkipped(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "inactive", SubState: "dead"}, nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Contains(t, result.Inactive, "foo.service")
	assert.Empty(t, result.Unhealthy)
	assert.Empty(t, result.Restarted)
	assert.Empty(t, result.Errors)
	assert.Contains(t, result.Statuses, "foo.service")
	assert.Equal(t, applier.UnitStatus{ActiveState: "inactive", SubState: "dead"}, result.Statuses["foo.service"])
}

func TestEnforceSkipsSecretsAndManifests(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	// No expectations — no units should be checked
	c := New(sys)

	st := state.NewState()
	st.ManagedFiles["secret:my_secret"] = state.ManagedFile{Hash: "sha256:abc", Category: "secret"}
	st.ManagedFiles["/var/lib/picolet/manifests/app/deployment.yml"] = state.ManagedFile{Hash: "sha256:def", Category: "manifest"}
	// no quadlet units → ServiceNames stays empty

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Empty(t, result.Healthy)
	assert.Empty(t, result.Unhealthy)
}

func TestEnforceHandlesCheckError(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{}, assert.AnError)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Errors, 1)
}

func TestEnforceRestartCooldown(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	// GetUnitStatus called twice (two Enforce calls), unit is unhealthy both times
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "auto-restart"}, nil).Times(2)
	// RestartUnit should only be called once (cooldown prevents second restart)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(nil).Once()

	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	// First enforce: restarts
	result1, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result1.Unhealthy, 1)
	assert.Equal(t, []string{"foo.service"}, result1.Restarted)
	assert.Empty(t, result1.Skipped)

	// Second enforce immediately: cooldown prevents restart
	result2, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result2.Unhealthy, 1)
	assert.Empty(t, result2.Errors)
	assert.Empty(t, result2.Restarted)
	assert.Equal(t, []string{"foo.service"}, result2.Skipped)
}

func TestEnforceFailedUnitPopulatesStatus(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	status, ok := result.Statuses["foo.service"]
	require.True(t, ok, "status should be populated for queried unit")
	assert.Equal(t, "failed", status.ActiveState)
	assert.Equal(t, "failed", status.SubState)
}

func TestEnforcePrunesRemovedUnitCooldowns(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	c := New(sys)
	c.lastRestart["foo.service"] = time.Now()
	c.lastRestart["removed.service"] = time.Now()

	st := &state.State{
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}
	_, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Contains(t, c.lastRestart, "foo.service")
	assert.NotContains(t, c.lastRestart, "removed.service")
}

func TestEnforceAddsPendingUnitOnRestartFailure(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(assert.AnError)

	c := New(sys)
	st := &state.State{
		AppliedSHA: "sha-abc",
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Errors, 1)
	require.Contains(t, st.PendingUnits, "foo.service")
	pu := st.PendingUnits["foo.service"]
	assert.Equal(t, 1, pu.Attempts)
	assert.Equal(t, "sha-abc", pu.SHA)
	assert.False(t, pu.FirstFailedAt.IsZero())
	assert.False(t, pu.LastAttemptAt.IsZero())
}

func TestEnforceClearsPendingUnitWhenHealthy(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	c := New(sys)
	st := &state.State{
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
		PendingUnits: map[string]state.PendingUnit{
			"foo.service": {SHA: "sha-abc", Attempts: 12, FirstFailedAt: time.Now(), LastAttemptAt: time.Now()},
		},
	}

	_, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Empty(t, st.PendingUnits, "a healthy unit must clear its pending record")
}

func TestEnforceClearsPendingUnitWhenInactive(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "inactive", SubState: "dead"}, nil)

	c := New(sys)
	st := &state.State{
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
		PendingUnits: map[string]state.PendingUnit{
			"foo.service": {SHA: "sha-abc", Attempts: 9, FirstFailedAt: time.Now(), LastAttemptAt: time.Now()},
		},
	}

	_, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Empty(t, st.PendingUnits, "an inactive unit is no longer retried, so its pending record must clear")
}

func TestEnforcePersistentCooldownAcrossNewChecker(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil)
	// No RestartUnit expectation: the persisted cooldown must suppress the restart.

	// A fresh Checker simulates a picolet process restart — its in-memory
	// lastRestart map is empty.
	c := New(sys)
	st := &state.State{
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
		PendingUnits: map[string]state.PendingUnit{
			"foo.service": {SHA: "sha-abc", Attempts: 3, FirstFailedAt: time.Now().Add(-time.Hour), LastAttemptAt: time.Now()},
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{"foo.service"}, result.Skipped)
	assert.Empty(t, result.Restarted)
}

func TestEnforcePrunesPendingUnitsForRemovedUnits(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	c := New(sys)
	st := &state.State{
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
		PendingUnits: map[string]state.PendingUnit{
			"removed.service": {SHA: "sha-abc", Attempts: 5, FirstFailedAt: time.Now(), LastAttemptAt: time.Now()},
		},
	}

	_, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.NotContains(t, st.PendingUnits, "removed.service", "record for an unmanaged unit must be pruned")
}

func TestEnforceIncrementsPendingUnitAttempts(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "foo.service").Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil).Times(2)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(assert.AnError).Times(2)

	c := New(sys)
	st := &state.State{
		AppliedSHA: "sha-abc",
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	_, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	firstFailed := st.PendingUnits["foo.service"].FirstFailedAt

	// Age both the in-memory and persisted cooldowns past restartCooldown so the
	// second Enforce attempts another restart.
	old := time.Now().Add(-restartCooldown - time.Minute)
	c.lastRestart["foo.service"] = old
	pu := st.PendingUnits["foo.service"]
	pu.LastAttemptAt = old
	st.PendingUnits["foo.service"] = pu

	_, err = c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Equal(t, 2, st.PendingUnits["foo.service"].Attempts)
	assert.True(t, st.PendingUnits["foo.service"].FirstFailedAt.Equal(firstFailed), "FirstFailedAt must be preserved across attempts")
}

func TestAllFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hr   CheckResult
		want bool
	}{
		{"all errors", CheckResult{Errors: []error{assert.AnError, assert.AnError}}, true},
		{"mixed", CheckResult{Healthy: []string{"a"}, Errors: []error{assert.AnError}}, false},
		{"all healthy", CheckResult{Healthy: []string{"a"}}, false},
		{"empty", CheckResult{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.hr.AllFailed())
		})
	}
}
