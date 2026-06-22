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
