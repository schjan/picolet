package applier_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

const timerWithInstall = "# Managed by picolet\n[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n"

func systemdChangeset(changes ...reconciler.Change) *reconciler.Changeset {
	cs := &reconciler.Changeset{Summary: map[reconciler.Action]int{}}
	for _, c := range changes {
		cs.Changes = append(cs.Changes, c)
		cs.Summary[c.Action]++
	}
	return cs
}

func TestApplySystemdTimerCreate(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().EnableUnit(mock.Anything, "foo.timer").Return(nil)
	sys.EXPECT().StartUnit(mock.Anything, "foo.timer").Return(nil)
	// RestartUnit must NOT be called for a passive-unit create.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/foo.timer", Category: "systemd",
		Action: reconciler.ActionCreate, NewContent: timerWithInstall, ServiceName: "foo.timer",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Empty(t, result.RestartedUnits)
	assert.Contains(t, fw.written, "/etc/systemd/system/foo.timer")
	assert.ElementsMatch(t, []applier.SystemdUnitOp{
		{Unit: "foo.timer", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
		{Unit: "foo.timer", Operation: applier.SystemdOpStart, Result: applier.SystemdOpResultSuccess},
	}, result.SystemdUnitOps)
}

//nolint:dupl // near-identical op-assertion shape to the raw-oneshot-with-install test; distinct scenarios
func TestApplySystemdTimerUpdate(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().EnableUnit(mock.Anything, "foo.timer").Return(nil)
	// Update restarts (not starts) so a changed schedule is reloaded.
	sys.EXPECT().RestartUnit(mock.Anything, "foo.timer").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/foo.timer", Category: "systemd",
		Action: reconciler.ActionUpdate, NewContent: timerWithInstall, ServiceName: "foo.timer",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.ElementsMatch(t, []applier.SystemdUnitOp{
		{Unit: "foo.timer", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
		{Unit: "foo.timer", Operation: applier.SystemdOpRestart, Result: applier.SystemdOpResultSuccess},
	}, result.SystemdUnitOps)
}

func TestApplySystemdTimerDelete(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().StopUnit(mock.Anything, "foo.timer").Return(nil)
	sys.EXPECT().DisableUnit(mock.Anything, "foo.timer").Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	const path = "/etc/systemd/system/foo.timer"
	cs := systemdChangeset(reconciler.Change{
		DestPath: path, Category: "systemd", Action: reconciler.ActionDelete, ServiceName: "foo.timer",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, []string{path}, fw.removed)
	assert.Contains(t, result.SystemdUnitOps, applier.SystemdUnitOp{
		Unit: "foo.timer", Operation: applier.SystemdOpDisable, Result: applier.SystemdOpResultSuccess,
	})
}

func TestApplySystemdOneshotNoInstall(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	// A timer-triggered oneshot has no [Install]: write + daemon-reload only.
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/maintenance.service", Category: "systemd",
		Action:      reconciler.ActionCreate,
		NewContent:  "# Managed by picolet\n[Service]\nType=oneshot\nExecStart=/bin/true\n",
		ServiceName: "maintenance.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Empty(t, result.SystemdUnitOps)
}

func TestApplySystemdDaemonServiceWithInstall(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().EnableUnit(mock.Anything, "daemon.service").Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "daemon.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/daemon.service", Category: "systemd",
		Action:      reconciler.ActionCreate,
		NewContent:  "# Managed by picolet\n[Service]\nExecStart=/usr/bin/daemon\n\n[Install]\nWantedBy=multi-user.target\n",
		ServiceName: "daemon.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.ElementsMatch(t, []applier.SystemdUnitOp{
		{Unit: "daemon.service", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
		{Unit: "daemon.service", Operation: applier.SystemdOpRestart, Result: applier.SystemdOpResultSuccess},
	}, result.SystemdUnitOps)
}

const quadletOneshot = "[Container]\nImage=job\n\n[Service]\nType=oneshot\n"

// A quadlet Type=oneshot whose generated .service is timer-triggered must not be
// restarted on a content edit — that would run the job. The restart is recorded
// as skipped instead.
func TestApplyQuadletOneshotTimerTriggeredNotRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "job.service").Return(applier.UnitStatus{
		ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot",
		TriggeredBy: []string{"job.timer"},
	}, nil)
	// RestartUnit must NOT be called.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/containers/systemd/picolet/job.container", Category: "container",
		Action: reconciler.ActionUpdate, NewContent: quadletOneshot, ServiceName: "job.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.RestartedUnits)
	assert.Empty(t, result.FailedRestartUnits)
	assert.Equal(t, []string{"job.service"}, result.SkippedRestarts())
	assert.Contains(t, result.SystemdUnitOps, applier.SystemdUnitOp{
		Unit: "job.service", Operation: applier.SystemdOpRestart, Result: applier.SystemdOpResultSkipped,
	})
}

// A quadlet Type=oneshot with no timer edge (nothing re-invokes it) is restarted
// like any other changed unit.
func TestApplyQuadletOneshotNoTriggerRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "job.service").Return(applier.UnitStatus{
		ActiveState: "active", SubState: "exited", ServiceType: "oneshot",
	}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "job.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/containers/systemd/picolet/job.container", Category: "container",
		Action: reconciler.ActionUpdate, NewContent: quadletOneshot, ServiceName: "job.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, []string{"job.service"}, result.RestartedUnits)
	assert.Empty(t, result.SkippedRestarts())
}

// When the pre-restart status check fails, the quadlet one-shot is not restarted
// (fail closed), the error surfaces on result.Errors, and the restart op is
// recorded as "error" — NOT "skipped" (that bucket is for deliberate timer gating)
// and NOT in FailedRestartUnits (so it does not trip the 3-strike gate).
func TestApplyQuadletOneshotStatusCheckErrorNotRestarted(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "job.service").Return(applier.UnitStatus{}, assert.AnError)
	// RestartUnit must NOT be called.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/containers/systemd/picolet/job.container", Category: "container",
		Action: reconciler.ActionUpdate, NewContent: quadletOneshot, ServiceName: "job.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.RestartedUnits)
	assert.Empty(t, result.FailedRestartUnits)
	assert.Empty(t, result.SkippedRestarts(), "a status-check failure is not a deliberate skip")
	assert.Len(t, result.Errors, 1)
	assert.Contains(t, result.SystemdUnitOps, applier.SystemdUnitOp{
		Unit: "job.service", Operation: applier.SystemdOpRestart, Result: applier.SystemdOpResultError,
	})
}

const rawOneshotWithInstall = "# Managed by picolet\n[Service]\nType=oneshot\nExecStart=/bin/true\n\n[Install]\nWantedBy=multi-user.target\n"

// A raw one-shot with [Install] is a first-boot provisioning job: on create it is
// enabled and started once (enable --now).
//
//nolint:dupl // near-identical op-assertion shape to the timer-update test; distinct scenarios
func TestApplySystemdOneshotWithInstallCreate(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().EnableUnit(mock.Anything, "provision.service").Return(nil)
	sys.EXPECT().StartUnit(mock.Anything, "provision.service").Return(nil)
	// RestartUnit must NOT be called.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/provision.service", Category: "systemd",
		Action: reconciler.ActionCreate, NewContent: rawOneshotWithInstall, ServiceName: "provision.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.ElementsMatch(t, []applier.SystemdUnitOp{
		{Unit: "provision.service", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
		{Unit: "provision.service", Operation: applier.SystemdOpStart, Result: applier.SystemdOpResultSuccess},
	}, result.SystemdUnitOps)
}

// On update the same job is only re-enabled, never re-run — re-executing on edit
// is the reported defect.
func TestApplySystemdOneshotWithInstallUpdate(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().EnableUnit(mock.Anything, "provision.service").Return(nil)
	// Neither StartUnit nor RestartUnit may be called.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/systemd/system/provision.service", Category: "systemd",
		Action: reconciler.ActionUpdate, NewContent: rawOneshotWithInstall, ServiceName: "provision.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, []applier.SystemdUnitOp{
		{Unit: "provision.service", Operation: applier.SystemdOpEnable, Result: applier.SystemdOpResultSuccess},
	}, result.SystemdUnitOps)
}

// Quadlet container changes must never be enabled/disabled — quadlet realizes
// [Install] itself and systemd refuses to enable generated units.
func TestApplyContainerDoesNotEnable(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	// EnableUnit/DisableUnit are NOT set up: the mock fails the test if either is called.
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := systemdChangeset(reconciler.Change{
		DestPath: "/etc/containers/systemd/app.container", Category: "container",
		Action: reconciler.ActionUpdate, NewContent: "[Container]\nImage=app\n", ServiceName: "app.service",
	})

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.SystemdUnitOps)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}
