package health

import (
	"context"
	"testing"
	"testing/synctest"
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

func TestEnforcePassiveTimerWaitingNotRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	// A timer at rest: active (waiting). Healthy; never restarted.
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.timer").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "waiting", UnitFileState: "enabled"}, nil)
	// RestartUnit must NOT be called.

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/systemd/system/backup.timer": {Hash: "sha256:abc", Category: "systemd"},
		},
		ServiceNames: map[string]string{
			"/etc/systemd/system/backup.timer": "backup.timer",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{"backup.timer"}, result.Healthy)
	assert.Empty(t, result.Unhealthy)
	assert.Empty(t, result.Restarted)
}

func TestEnforcePassiveTimerFailedNotRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	// Even a failed timer is reported, not restarted — restart semantics for
	// timers are odd, and health-enforce must not churn them.
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.timer").
		Return(applier.UnitStatus{ActiveState: "failed", SubState: "dead"}, nil)
	// RestartUnit must NOT be called.

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/systemd/system/backup.timer": {Hash: "sha256:abc", Category: "systemd"},
		},
		ServiceNames: map[string]string{
			"/etc/systemd/system/backup.timer": "backup.timer",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Contains(t, result.Inactive, "backup.timer")
	assert.Empty(t, result.Unhealthy)
	assert.Empty(t, result.Restarted)
}

// A genuinely-failed managed service (not a passive unit) is still restarted.
func TestEnforceFailedServiceStillRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").
		Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/app.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/app.container": "app.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{"app.service"}, result.Restarted)
}

// A failed one-shot systemd owns (timer-triggered or static raw) is reported but
// never restarted; a failed daemon — even one with a timer attached, or a static
// helper pulled in by Requires= — still is. mockery's NewMockSystemdManager fails
// on an unexpected RestartUnit, which is how "must not restart" is asserted.
func TestEnforceExternallyActivatedOneshots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		unitFileState string
		triggeredBy   []string
		serviceType   string
		wantRestart   bool
	}{
		{"static raw oneshot", "static", nil, "oneshot", false},
		{"timer-triggered quadlet oneshot", "generated", []string{"job.timer"}, "oneshot", false},
		{"daemon with a timer attached", "generated", []string{"app.timer"}, "notify", true},
		{"Requires=-pulled helper daemon", "static", nil, "simple", true},
		// A socket-triggered static oneshot is exempted by the static clause, not the
		// trigger clause (sockets are not a reliable re-trigger) — still not restarted.
		{"socket-triggered static oneshot", "static", []string{"job.socket"}, "oneshot", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sys := appliermocks.NewMockSystemdManager(t)
			sys.EXPECT().GetUnitStatus(mock.Anything, "job.service").Return(applier.UnitStatus{
				ActiveState:   "failed",
				SubState:      "failed",
				UnitFileState: tc.unitFileState,
				TriggeredBy:   tc.triggeredBy,
				ServiceType:   tc.serviceType,
			}, nil)
			if tc.wantRestart {
				sys.EXPECT().RestartUnit(mock.Anything, "job.service").Return(nil)
			}

			c := New(sys)
			// An old attempt so the restart cooldown never suppresses the daemon rows.
			old := time.Now().Add(-time.Hour)
			st := &state.State{
				ServiceNames: map[string]string{
					"/etc/containers/systemd/picolet/job.service": "job.service",
				},
				PendingUnits: map[string]state.PendingUnit{
					"job.service": {SHA: "sha", Attempts: 2, FirstFailedAt: old, LastAttemptAt: old},
				},
			}

			result, err := c.Enforce(context.Background(), st)
			require.NoError(t, err)
			assert.Contains(t, result.Unhealthy, "job.service")
			if tc.wantRestart {
				assert.Equal(t, []string{"job.service"}, result.Restarted)
				assert.Empty(t, result.ExternallyActivated)
				return
			}
			assert.Empty(t, result.Restarted)
			assert.Equal(t, []string{"job.service"}, result.ExternallyActivated)
			assert.NotContains(t, st.PendingUnits, "job.service",
				"an externally-activated unit is never retried, so its pending record must clear")
		})
	}
}

// Run bookkeeping must be classified on every state path. A one-shot that works
// spends its life *inactive*, so classifying only failed units — where
// ExternallyActivated is consulted — would never record a healthy job.
func TestEnforceClassifiesTimerJobsOnEveryPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		status      applier.UnitStatus
		wantTracked bool
	}{
		{
			"completed timer one-shot (inactive)",
			applier.UnitStatus{ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot", TriggeredBy: []string{"job.timer"}},
			true,
		},
		{
			"running timer one-shot (activating)",
			applier.UnitStatus{ActiveState: "activating", SubState: "start", ServiceType: "oneshot", TriggeredBy: []string{"job.timer"}},
			true,
		},
		{
			"failed timer one-shot",
			applier.UnitStatus{ActiveState: "failed", SubState: "failed", ServiceType: "oneshot", TriggeredBy: []string{"job.timer"}},
			true,
		},
		{
			"static one-shot with no timer has no schedule to be late against",
			applier.UnitStatus{ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot", UnitFileState: "static"},
			false,
		},
		{
			"daemon with a timer attached is not a job",
			applier.UnitStatus{ActiveState: "active", SubState: "running", ServiceType: "notify", TriggeredBy: []string{"job.timer"}},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sys := appliermocks.NewMockSystemdManager(t)
			sys.EXPECT().GetUnitStatus(mock.Anything, "job.service").Return(tc.status, nil)
			// No RestartUnit expectation: every row here is either healthy or an
			// externally-activated one-shot, so the strict mock fails the test if the
			// checker tries to restart it.

			c := New(sys)
			st := &state.State{ServiceNames: map[string]string{
				"/etc/systemd/user/job.service": "job.service",
			}}

			result, err := c.Enforce(context.Background(), st)
			require.NoError(t, err)
			if tc.wantTracked {
				assert.Equal(t, []string{"job.service"}, result.TimerJobs)
				return
			}
			assert.Empty(t, result.TimerJobs)
		})
	}
}

// A .timer earns its trigger series through the one-shot it fires, so a pass that
// could not query that one-shot tracks nothing — recording a zero observation over
// the timer's retained trigger time would make the series flap. Managed still
// lists the whole Fleet unit set, errored units included, or a D-Bus hiccup would
// prune the retained records of everything it failed to query.
func TestEnforceTimerNeedsQueryableJob(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.timer").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "waiting", UnitFileState: "enabled"}, nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.service").Return(applier.UnitStatus{}, assert.AnError)
	sys.EXPECT().GetUnitStatus(mock.Anything, "web.service").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	c := New(sys)
	st := &state.State{ServiceNames: map[string]string{
		"/etc/systemd/user/backup.timer":                "backup.timer",
		"/etc/systemd/user/backup.service":              "backup.service",
		"/etc/containers/systemd/picolet/web.container": "web.service",
	}}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Empty(t, result.TimerJobs,
		"the one-shot that would qualify its timer could not be queried this pass")
	assert.Equal(t, []string{"backup.service", "backup.timer", "web.service"}, result.Managed)
}

// A .timer is tracked through the job it fires, and only that job: a timer that
// activates a daemon has no scheduled-run history to report.
func TestEnforceTracksTimersOfJobsOnly(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.timer").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "waiting", UnitFileState: "enabled"}, nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "backup.service").Return(applier.UnitStatus{
		ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot",
		TriggeredBy: []string{"backup.timer"},
	}, nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "churn.timer").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "waiting", UnitFileState: "enabled"}, nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "web.service").Return(applier.UnitStatus{
		ActiveState: "active", SubState: "running", ServiceType: "notify",
		TriggeredBy: []string{"churn.timer"},
	}, nil)

	c := New(sys)
	st := &state.State{ServiceNames: map[string]string{
		"/etc/systemd/user/backup.timer":                "backup.timer",
		"/etc/systemd/user/backup.service":              "backup.service",
		"/etc/systemd/user/churn.timer":                 "churn.timer",
		"/etc/containers/systemd/picolet/web.container": "web.service",
	}}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Equal(t, []string{"backup.service", "backup.timer"}, result.TimerJobs,
		"only the one-shot and the timer that fires it carry run bookkeeping")
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
	synctest.Test(t, func(t *testing.T) {
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

		_, err := c.Enforce(t.Context(), st)
		require.NoError(t, err)
		firstFailed := st.PendingUnits["foo.service"].FirstFailedAt

		// testing/synctest advances the fake clock instantly, so this ages both
		// the in-memory and persisted cooldowns without mutating implementation
		// internals or sleeping in real time.
		time.Sleep(restartCooldown + time.Nanosecond)

		_, err = c.Enforce(t.Context(), st)
		require.NoError(t, err)
		assert.Equal(t, 2, st.PendingUnits["foo.service"].Attempts)
		assert.True(t, st.PendingUnits["foo.service"].FirstFailedAt.Equal(firstFailed), "FirstFailedAt must be preserved across attempts")
	})
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
