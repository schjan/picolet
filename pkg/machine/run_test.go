package machine_test

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

// userDirs and rootDirs are the four directories the Agent quadlet
// bind-mounts, for a rootless Host (below the home) and the rootful one.
var (
	userDirs = []struct {
		rel  string
		mode fs.FileMode
	}{
		{".config/picolet/secrets", 0o700},
		{".local/share/picolet", 0o700},
		{".config/containers/systemd", 0o755},
		{".config/systemd/user", 0o755},
	}
	rootDirs = []struct {
		path string
		mode fs.FileMode
	}{
		{"/etc/picolet/secrets", 0o700},
		{"/var/lib/picolet-system", 0o700},
		{"/etc/containers/systemd", 0o755},
		{"/etc/systemd/system", 0o755},
	}
)

// runMachine bootstraps Machine vps-1 of the example fleet as root and
// returns the output with the checkout path replaced by <repo>.
func runMachine(t *testing.T, ops machine.HostOps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine: "vps-1",
		RepoDir: exampleFleet,
		Env:     rootOnLinux(),
		Stdout:  &out,
	}, ops)
	return strings.ReplaceAll(out.String(), exampleFleetAbs(t), "<repo>"), err
}

// expectFreshUser sets ops up as a Machine without u: every check of u's
// steps fails until the step is applied, and every step is applied once.
func expectFreshUser(ops *mocks.MockHostOps, u machine.User) {
	ops.EXPECT().LookupUser(u.Name).Return(machine.User{}, false, nil).Once()
	ops.EXPECT().CreateUser(mock.Anything, u.Name).Return(nil).Once()
	ops.EXPECT().LookupUser(u.Name).Return(u, true, nil)
	ops.EXPECT().SubIDRanges(u).Return(true, true, nil)
	ops.EXPECT().LingerEnabled(u).Return(false, nil)
	ops.EXPECT().EnableLinger(mock.Anything, u).Return(nil).Once()
	ops.EXPECT().UserManagerRunning(u).Return(false, nil).Once()
	ops.EXPECT().WaitUserManager(mock.Anything, u).Return(nil).Once()
	ops.EXPECT().UserManagerRunning(u).Return(true, nil)
	ops.EXPECT().UserUnitState(mock.Anything, u, "podman.socket").Return(machine.UnitState{}, nil)
	ops.EXPECT().EnableUserUnit(mock.Anything, u, "podman.socket").Return(nil).Once()
	for _, d := range userDirs {
		ops.EXPECT().Stat(u.Home+"/"+d.rel).Return(machine.PathInfo{}, nil)
		ops.EXPECT().EnsureDir(u.Home, d.rel, machine.Owner{UID: u.UID, GID: u.GID}, d.mode).Return(nil).Once()
	}
}

// expectBootstrappedUser sets ops up as a Machine where every step of u is
// done, its podman.socket in state socket.
func expectBootstrappedUser(ops *mocks.MockHostOps, u machine.User, socket machine.UnitState) {
	ops.EXPECT().LookupUser(u.Name).Return(u, true, nil)
	ops.EXPECT().SubIDRanges(u).Return(true, true, nil)
	ops.EXPECT().LingerEnabled(u).Return(true, nil)
	ops.EXPECT().UserManagerRunning(u).Return(true, nil)
	ops.EXPECT().UserUnitState(mock.Anything, u, "podman.socket").Return(socket, nil)
	for _, d := range userDirs {
		ops.EXPECT().Stat(u.Home+"/"+d.rel).Return(dir(u.UID, u.GID, d.mode), nil)
	}
}

// running is the state of a unit that is enabled and active.
var running = machine.UnitState{Enabled: true, Active: true}

func expectBootstrappedRootful(ops *mocks.MockHostOps, socket machine.UnitState) {
	ops.EXPECT().SystemUnitState(mock.Anything, "podman.socket").Return(socket, nil)
	for _, d := range rootDirs {
		ops.EXPECT().Stat(d.path).Return(dir(0, 0, d.mode), nil)
	}
}

// A first run on a fresh Machine applies every step in plan order, each
// once. Two are found done: subids is only verified (useradd allocated the
// ranges), and the shared checkout, made readable for pi, is already
// readable for runner. The rootful Host has no user steps and gets the
// system socket. Every Agent is then started and found healthy.
func TestRunFreshMachine(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectFreshUser(ops, pi)
	expectFreshUser(ops, runner)
	ops.EXPECT().SystemUnitState(mock.Anything, "podman.socket").Return(machine.UnitState{}, nil)
	ops.EXPECT().EnableSystemUnit(mock.Anything, "podman.socket").Return(nil).Once()
	for _, d := range rootDirs {
		ops.EXPECT().Stat(d.path).Return(machine.PathInfo{}, nil)
		ops.EXPECT().EnsureDir("/", d.path[1:], machine.Owner{}, d.mode).Return(nil).Once()
	}
	repo := exampleFleetAbs(t)
	ops.EXPECT().WorldReadableTree(repo).Return(false, nil).Once()
	ops.EXPECT().MakeWorldReadable(repo).Return(nil).Once()
	ops.EXPECT().WorldReadableTree(repo).Return(true, nil)
	expectAgentsStarted(t, ops)

	out, err := runMachine(t, ops)
	require.NoError(t, err)
	goldie.New(t).Assert(t, "run-fresh-machine", []byte(out))
}

// A second run finds every step done and changes nothing but re-running the
// per-Host bootstraps, which leave a current Agent alone: the mock has no
// other write expectations.
func TestRunBootstrappedMachine(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	expectAgentsStarted(t, ops)

	out, err := runMachine(t, ops)
	require.NoError(t, err)
	goldie.New(t).Assert(t, "run-bootstrapped-machine", []byte(out))
}

// An enabled podman.socket that is not running is not done: the run starts
// it, the user's and the system's.
func TestRunStartsEnabledStoppedSocket(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	stopped := machine.UnitState{Enabled: true}
	expectBootstrappedUser(ops, pi, stopped)
	ops.EXPECT().EnableUserUnit(mock.Anything, pi, "podman.socket").Return(nil).Once()
	expectBootstrappedUser(ops, runner, running)
	expectBootstrappedRootful(ops, stopped)
	ops.EXPECT().EnableSystemUnit(mock.Anything, "podman.socket").Return(nil).Once()
	ops.EXPECT().WorldReadableTree(exampleFleetAbs(t)).Return(true, nil)
	expectAgentsStarted(t, ops)

	out, err := runMachine(t, ops)
	require.NoError(t, err)
	require.Regexp(t, `applied\s+vps-1/podman-socket`, out)
	require.Regexp(t, `applied\s+vps-1-system/podman-socket`, out)
	require.Contains(t, out, "5 applied, 23 already done")
}

// A user without subordinate ID ranges stops its Host at the check, with the
// command that adds them; the other Hosts are bootstrapped and their Agents
// started, and the run fails. Nothing of runner past the check is touched.
func TestRunMissingSubIDsStopsOnlyThatHost(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedUser(ops, pi, running)
	ops.EXPECT().LookupUser("runner").Return(runner, true, nil)
	ops.EXPECT().SubIDRanges(runner).Return(false, false, nil)
	expectBootstrappedRootful(ops, running)
	ops.EXPECT().WorldReadableTree(exampleFleetAbs(t)).Return(true, nil)
	expectUserAgentStarted(ops, exampleFleetAbs(t), pi, "vps-1", "127.0.0.1:9417")
	expectRootfulAgentStarted(ops, exampleFleetAbs(t))

	out, err := runMachine(t, ops)
	require.EqualError(t, err, "vps-1-runner/subids: no subuid/subgid range; add one: "+
		"usermod --add-subuids 100000-165535 --add-subgids 100000-165535 runner")
	goldie.New(t).Assert(t, "run-missing-subids", []byte(out))
}

// rootOnLinux is the Environment a run accepts: root on Linux, outside a
// container, with Podman installed.
func rootOnLinux() machine.Environment {
	return machine.Environment{GOOS: "linux", Root: true, Podman: true}
}

// A run that cannot succeed stops before its first step: the mock has no
// expectations, so any check or write fails the test.
func TestRunFailsFast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  machine.Config
		want string
	}{
		{
			name: "not root",
			cfg:  machine.Config{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{GOOS: "linux", Podman: true}},
			want: "bootstrap machine must run as root",
		},
		{
			name: "Podman missing",
			cfg:  machine.Config{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{GOOS: "linux", Root: true}},
			want: "podman is not installed",
		},
		{
			name: "unknown Machine",
			cfg:  machine.Config{Machine: "vps-2", RepoDir: exampleFleet, Env: rootOnLinux()},
			want: `no Host in the Fleet runs on machine "vps-2"`,
		},
		{
			name: "inside a container",
			cfg: machine.Config{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{
				GOOS: "linux", Root: true, Podman: true, InContainer: true,
			}},
			want: "not inside a container",
		},
		{
			name: "missing repo dir",
			cfg:  machine.Config{Machine: "vps-1", Env: rootOnLinux()},
			want: "--repo-dir is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			tt.cfg.Stdout = &out
			err := machine.Run(context.Background(), tt.cfg, mocks.NewMockHostOps(t))
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out.String(), "a rejected run prints nothing")
		})
	}
}
