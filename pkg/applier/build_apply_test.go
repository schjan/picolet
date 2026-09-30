package applier_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/status"
)

const (
	testContainerfile = "/var/lib/picolet/files/app/Containerfile"
	testBuildUnit     = "[Build]\nImageTag=localhost/app:latest\nFile=" + testContainerfile + "\nSetWorkingDirectory=file\n"
)

// buildDeps is the dependency map validation computes for app.container with
// Image=app.build and Pod=web.pod.
var buildDeps = map[string]status.UnitDependencies{
	"app.service":     {Requires: []string{"app-build.service", "web-pod.service"}, After: []string{"app-build.service"}},
	"web-pod.service": {Wants: []string{"app.service"}},
}

func buildChange(content string, action reconciler.Action) reconciler.Change {
	return reconciler.Change{
		DestPath: testQuadletDir + "app.build", Category: config.CategoryBuild, Action: action,
		NewContent: content, ServiceName: "app-build.service",
	}
}

func dataFileChange(path string, action reconciler.Action) reconciler.Change {
	return reconciler.Change{DestPath: path, Category: config.CategoryFile, Action: action, NewContent: "FROM alpine\n"}
}

// imagePodman returns a Podman mock whose image tags keep their IDs, for
// tests where every build succeeds or no tag needs restoring.
func imagePodman(t *testing.T) *appliermocks.MockPodmanClient {
	t.Helper()
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ImageID(mock.Anything, mock.Anything).Return("id", nil).Maybe()
	return pod
}

// recordingSystemd returns a systemd mock whose unit operations append
// "<op> <unit>" to the returned log, and succeed unless failing names the op.
// Units report the status in statuses, active by default.
func recordingSystemd(t *testing.T, statuses map[string]applier.UnitStatus, failing ...string) (*appliermocks.MockSystemdManager, *[]string) {
	t.Helper()
	sys := appliermocks.NewMockSystemdManager(t)
	var log []string
	record := func(op string) func(context.Context, string) error {
		return func(_ context.Context, unit string) error {
			call := op + " " + unit
			log = append(log, call)
			if slices.Contains(failing, call) {
				return assert.AnError
			}
			return nil
		}
	}
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RunBuildUnit(mock.Anything, mock.Anything).RunAndReturn(record("build")).Maybe()
	sys.EXPECT().RestartUnitIgnoringDependencies(mock.Anything, mock.Anything).RunAndReturn(record("restart-nodeps")).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, unit string) (applier.UnitStatus, error) {
		if st, ok := statuses[unit]; ok {
			return st, nil
		}
		return applier.UnitStatus{ActiveState: "active"}, nil
	}).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).RunAndReturn(record("restart")).Maybe()
	return sys, &log
}

// rebuildCases: a change to a .build unit or a file it builds from runs the
// build first (a start, which leaves its consumers alone) and only then
// restarts the running consumers without their dependencies, so the build
// runs once.
var rebuildCases = []struct {
	name    string
	changes []reconciler.Change
	want    []string
}{
	{
		name: "Containerfile changed",
		changes: []reconciler.Change{
			buildChange(testBuildUnit, reconciler.ActionNoop),
			dataFileChange(testContainerfile, reconciler.ActionUpdate),
		},
		want: []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		name:    ".build unit changed",
		changes: []reconciler.Change{buildChange(testBuildUnit+"BuildArg=V=2\n", reconciler.ActionUpdate)},
		want:    []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		name: "file in the build context changed",
		changes: []reconciler.Change{
			buildChange(testBuildUnit, reconciler.ActionNoop),
			dataFileChange("/var/lib/picolet/files/app/src/main.go", reconciler.ActionDelete),
		},
		want: []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		name: "context given as a path",
		changes: []reconciler.Change{
			buildChange("[Build]\nImageTag=localhost/app\nFile=Containerfile\nSetWorkingDirectory=/srv/app\n", reconciler.ActionNoop),
			dataFileChange("/srv/app/Containerfile", reconciler.ActionUpdate),
		},
		want: []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		name: "context relative to the unit",
		changes: []reconciler.Change{
			buildChange("[Build]\nImageTag=localhost/app\nFile=Containerfile\nSetWorkingDirectory=ctx\n", reconciler.ActionNoop),
			dataFileChange(testQuadletDir+"ctx/Containerfile", reconciler.ActionUpdate),
		},
		want: []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		// podman build without a context argument uses the Containerfile's
		// directory.
		name: "file beside a Containerfile without SetWorkingDirectory=",
		changes: []reconciler.Change{
			buildChange("[Build]\nImageTag=localhost/app\nFile="+testContainerfile+"\n", reconciler.ActionNoop),
			dataFileChange("/var/lib/picolet/files/app/src/main.go", reconciler.ActionUpdate),
		},
		want: []string{"build app-build.service", "restart-nodeps app.service"},
	},
	{
		// =unit makes the unit's directory the working directory; an
		// absolute File= still takes the context from its own directory.
		name: "SetWorkingDirectory=unit beside an absolute Containerfile",
		changes: []reconciler.Change{
			buildChange("[Build]\nImageTag=localhost/app\nFile="+testContainerfile+"\nSetWorkingDirectory=unit\n", reconciler.ActionNoop),
			dataFileChange(testQuadletDir+"ctx/Containerfile", reconciler.ActionUpdate),
		},
		want: nil,
	},
	{
		name: "unrelated file changed",
		changes: []reconciler.Change{
			buildChange(testBuildUnit, reconciler.ActionNoop),
			dataFileChange("/var/lib/picolet/files/app2/Containerfile", reconciler.ActionUpdate),
			dataFileChange("/var/lib/picolet/files/app-Containerfile", reconciler.ActionUpdate),
		},
		want: nil,
	},
	{
		// The consumer's own change is restarted once, without its
		// dependencies (a normal restart would run the build again), after
		// the dependencies changed alongside it.
		name: "consumer changed with the build",
		changes: []reconciler.Change{
			buildChange(testBuildUnit, reconciler.ActionNoop),
			dataFileChange(testContainerfile, reconciler.ActionUpdate),
			containerChange("app", "[Container]\nImage=app.build\nNetwork=net.network\n", reconciler.ActionUpdate),
			{DestPath: testQuadletDir + "net.network", Category: config.CategoryNetwork, Action: reconciler.ActionCreate, NewContent: "[Network]\n", ServiceName: "net-network.service"},
		},
		want: []string{"build app-build.service", "restart net-network.service", "restart-nodeps app.service"},
	},
	{
		// Its pod's restart stops and starts it (Wants=): no second restart.
		name: "consumer's pod changed",
		changes: []reconciler.Change{
			buildChange(testBuildUnit, reconciler.ActionNoop),
			dataFileChange(testContainerfile, reconciler.ActionUpdate),
			podChange(reconciler.ActionUpdate),
		},
		want: []string{"build app-build.service", "restart web-pod.service"},
	},
	{
		name: "build deleted",
		changes: []reconciler.Change{
			{DestPath: testQuadletDir + "app.build", Category: config.CategoryBuild, Action: reconciler.ActionDelete, ServiceName: "app-build.service"},
			dataFileChange(testContainerfile, reconciler.ActionDelete),
		},
		want: nil,
	},
}

func TestApplyRebuildsOnInputChange(t *testing.T) {
	t.Parallel()
	for _, tt := range rebuildCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys, log := recordingSystemd(t, nil)
			sys.EXPECT().StopUnit(mock.Anything, "app-build.service").Return(nil).Maybe()
			a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
				applier.WithDependencies(buildDeps))

			_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: tt.changes})
			require.NoError(t, err)
			assert.Equal(t, tt.want, *log)
		})
	}
}

// A containerized Agent writes data files below its data dir while the
// .build names them by the path the Host sees: a changed Containerfile
// still rebuilds.
func TestApplyRebuildsOnInputChangeAtHostDataDir(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, nil)
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
		applier.WithDependencies(buildDeps), applier.WithHostDataDir("/var/lib/picolet", "/srv/picolet"))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange("[Build]\nImageTag=localhost/app\nFile=/srv/picolet/files/app/Containerfile\nSetWorkingDirectory=file\n", reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"build app-build.service", "restart-nodeps app.service"}, *log)
}

// A failed build fails the apply before anything is restarted: neither its
// consumers nor other changed units are touched, so the caller rolls back
// with the running services as they were.
func TestApplyFailedBuildRestartsNothing(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, nil, "build app-build.service")
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
		applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
		containerChange("app", "[Container]\nImage=app.build\n", reconciler.ActionUpdate),
		containerChange("other", "[Container]\nImage=other\n", reconciler.ActionUpdate),
	}})
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, []string{"build app-build.service"}, *log)
}

// A consumer that fails to restart after a successful build is a failed
// restart like any other: pending, retried by the health loop.
func TestApplyFailedConsumerRestartIsPending(t *testing.T) {
	t.Parallel()
	sys, _ := recordingSystemd(t, nil, "restart-nodeps app.service")
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
		applier.WithDependencies(buildDeps))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"app.service"}, result.FailedRestartUnits)
	assert.Equal(t, []string{"app-build.service"}, result.RestartedUnits)
}

// A consumer a timer runs (a scheduled one-shot job) is not run by a
// rebuild: its timer starts it on the new image next time.
func TestApplyRebuildLeavesTimerJobConsumer(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, map[string]applier.UnitStatus{
		"app.service": {ActiveState: "inactive", ServiceType: "oneshot", TriggeredBy: []string{"app.timer"}},
	})
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
		applier.WithDependencies(buildDeps))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"build app-build.service"}, *log)
	assert.Equal(t, []string{"app.service"}, result.SkippedRestarts())
}

// A consumer that is not running is started the ordinary way, with its
// dependencies: nothing runs that a second (cached) build could take down.
func TestApplyRebuildStartsStoppedConsumerWithDependencies(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, map[string]applier.UnitStatus{"app.service": {ActiveState: "failed"}})
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil, applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"build app-build.service", "restart app.service"}, *log)
}

// A restart hook on the build service (the pre-#127 workaround) is covered by
// the build that already ran: an ordinary restart of the build would
// propagate to its running consumers.
func TestApplyRebuildSubsumesBuildRestartHook(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, nil)
	hook := config.Hook{Name: "rebuild", Files: []string{"app/Containerfile"}, Unit: "app-build.service", Action: config.HookActionRestart}
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, []config.Hook{hook}, applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		{DestPath: testContainerfile, Category: config.CategoryFile, Action: reconciler.ActionUpdate, NewContent: "FROM alpine\n", RelPath: "app/Containerfile"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"build app-build.service", "restart-nodeps app.service"}, *log)
}

// The agent's own container consuming a rebuilt image is restarted (deferred,
// like any self restart) without its dependencies, so the build is not run
// again under the agent.
func TestApplyRebuildRestartsSelfConsumerWithoutDependencies(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().RunBuildUnit(mock.Anything, "app-build.service").Return(nil)
	done := make(chan struct{})
	sys.EXPECT().RestartUnitIgnoringDependencies(mock.Anything, "app.service").
		RunAndReturn(func(context.Context, string) error { close(done); return nil })
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil,
		applier.WithDependencies(buildDeps), applier.WithSelfUnits("app.service"))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
	}})
	require.NoError(t, err)
	assert.Contains(t, result.RestartedUnits, "app.service")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred self restart did not run")
	}
}

// A later build failing leaves no earlier image of the same Reconciliation
// updated: every tag is put back on the image it named before, so nothing
// built from inputs the rollback restores is left behind.
func TestApplyFailedBuildRestoresEarlierImageTags(t *testing.T) {
	t.Parallel()
	const webFile = "/var/lib/picolet/files/web/Containerfile"
	sys, log := recordingSystemd(t, nil, "build web-build.service")
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ImageID(mock.Anything, "localhost/app:latest").Return("old-app", nil).Once()
	pod.EXPECT().ImageID(mock.Anything, "localhost/web:latest").Return("", nil).Once()
	pod.EXPECT().ImageID(mock.Anything, "localhost/app:latest").Return("new-app", nil).Once()
	pod.EXPECT().ImageID(mock.Anything, "localhost/web:latest").Return("new-web", nil).Once()
	pod.EXPECT().ImageTag(mock.Anything, "old-app", "localhost/app:latest").Return(nil).Once()
	pod.EXPECT().ImageUntag(mock.Anything, "localhost/web:latest").Return(nil).Once()
	a := applier.New(sys, pod, newMemFileWriter(), false, nil, applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
		{
			DestPath: testQuadletDir + "web.build", Category: config.CategoryBuild, Action: reconciler.ActionNoop,
			NewContent:  "[Build]\nImageTag=localhost/web:latest\nFile=" + webFile + "\nSetWorkingDirectory=file\n",
			ServiceName: "web-build.service",
		},
		dataFileChange(webFile, reconciler.ActionUpdate),
	}})
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, []string{"build app-build.service", "build web-build.service"}, *log)
}

// Two builds writing the same tag: it is recorded and restored once, to the
// image it named before either build ran.
func TestApplyFailedBuildRestoresSharedTagOnce(t *testing.T) {
	t.Parallel()
	const webFile = "/var/lib/picolet/files/web/Containerfile"
	sys, _ := recordingSystemd(t, nil, "build web-build.service")
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().ImageID(mock.Anything, "localhost/app:latest").Return("old", nil).Once()
	pod.EXPECT().ImageID(mock.Anything, "localhost/app:latest").Return("new", nil).Once()
	pod.EXPECT().ImageTag(mock.Anything, "old", "localhost/app:latest").Return(nil).Once()
	a := applier.New(sys, pod, newMemFileWriter(), false, nil, applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionNoop),
		dataFileChange(testContainerfile, reconciler.ActionUpdate),
		{
			DestPath: testQuadletDir + "web.build", Category: config.CategoryBuild, Action: reconciler.ActionNoop,
			NewContent:  "[Build]\nImageTag=localhost/app:latest\nFile=" + webFile + "\nSetWorkingDirectory=file\n",
			ServiceName: "web-build.service",
		},
		dataFileChange(webFile, reconciler.ActionUpdate),
	}})
	require.ErrorIs(t, err, assert.AnError)
}

// First deploy: a consumer created with its build is not running yet, so it
// is started the ordinary way, pulling in its dependencies.
func TestApplyStartsConsumerCreatedWithItsBuild(t *testing.T) {
	t.Parallel()
	sys, log := recordingSystemd(t, map[string]applier.UnitStatus{"app.service": {ActiveState: "inactive"}})
	a := applier.New(sys, imagePodman(t), newMemFileWriter(), false, nil, applier.WithDependencies(buildDeps))

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		buildChange(testBuildUnit, reconciler.ActionCreate),
		containerChange("app", "[Container]\nImage=app.build\n", reconciler.ActionCreate),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"build app-build.service", "restart app.service"}, *log)
}
