package machine_test

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

const exampleFleet = "../../testdata/example-fleet"

// exampleFleetAbs is the checkout path the plan checks and renders: absolute,
// symlinks resolved.
func exampleFleetAbs(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs(exampleFleet)
	require.NoError(t, err)
	repo, err = filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return repo
}

var (
	pi     = machine.User{Name: "pi", UID: 1000, GID: 1000, Home: "/home/pi"}
	runner = machine.User{Name: "runner", UID: 1001, GID: 1001, Home: "/home/runner"}
)

func dir(uid, gid int, perm fs.FileMode) machine.PathInfo {
	return machine.PathInfo{Exists: true, UID: uid, GID: gid, Mode: fs.ModeDir | perm}
}

func linuxHost() machine.Environment {
	return machine.Environment{GOOS: "linux"}
}

// showPlan renders the plan for Machine vps-1 of the example fleet checked
// out at repoDir, with the checkout's resolved path replaced by <repo> so the
// golden is portable.
func showPlan(t *testing.T, repoDir string, ops machine.HostOps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := machine.ShowPlan(context.Background(), machine.PlanConfig{
		Machine: "vps-1",
		RepoDir: repoDir,
		Env:     linuxHost(),
		Stdout:  &out,
	}, ops)
	return strings.ReplaceAll(out.String(), exampleFleetAbs(t), "<repo>"), err
}

// expectFreshMachine sets ops up as a Machine nothing has been done to yet,
// planned as root: no user exists, so every user-level step is "would do"
// without a check.
func expectFreshMachine(t *testing.T, ops *mocks.MockHostOps) {
	t.Helper()
	ops.EXPECT().LookupUser("pi").Return(machine.User{}, false, nil)
	ops.EXPECT().LookupUser("runner").Return(machine.User{}, false, nil)
	ops.EXPECT().SystemUnitEnabled(mock.Anything, "podman.socket").Return(false, nil)
	for _, p := range []string{"/etc/picolet/secrets", "/var/lib/picolet-system", "/etc/containers/systemd"} {
		ops.EXPECT().Stat(p).Return(machine.PathInfo{}, nil)
	}
	ops.EXPECT().Stat("/etc/systemd/system").Return(dir(0, 0, 0o755), nil)
	ops.EXPECT().WorldReadableTree(exampleFleetAbs(t)).Return(false, nil).Times(2)
}

func TestShowPlanFreshMachine(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectFreshMachine(t, ops)

	out, err := showPlan(t, exampleFleet, ops)
	require.NoError(t, err)
	goldie.New(t).Assert(t, "fresh-machine", []byte(out))
}

// A checkout reached through a symlink is checked, and shown, as the tree the
// symlink points at: a walk of the link itself would see no tree.
func TestShowPlanResolvesSymlinkedCheckout(t *testing.T) {
	t.Parallel()
	link := filepath.Join(t.TempDir(), "fleet")
	require.NoError(t, os.Symlink(exampleFleetAbs(t), link))
	ops := mocks.NewMockHostOps(t)
	expectFreshMachine(t, ops)

	out, err := showPlan(t, link, ops)
	require.NoError(t, err)
	require.Contains(t, out, "Fleet checkout: <repo>\n")
}

// A partly bootstrapped Machine planned by an unprivileged operator: pi is
// set up but for two directories, runner lacks a subgid range and its home
// and session cannot be inspected without root.
func TestShowPlanPartialMachineUnprivileged(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)

	ops.EXPECT().LookupUser("pi").Return(pi, true, nil)
	ops.EXPECT().SubIDRanges(pi).Return(true, true, nil)
	ops.EXPECT().LingerEnabled(pi).Return(true, nil)
	ops.EXPECT().UserManagerRunning(pi).Return(true, nil)
	ops.EXPECT().UserUnitEnabled(mock.Anything, pi, "podman.socket").Return(true, nil)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets").Return(dir(1000, 1000, 0o700), nil)
	ops.EXPECT().Stat("/home/pi/.local/share/picolet").Return(dir(1000, 1000, 0o755), nil)
	ops.EXPECT().Stat("/home/pi/.config/containers/systemd").Return(dir(0, 0, 0o755), nil)
	ops.EXPECT().Stat("/home/pi/.config/systemd/user").Return(machine.PathInfo{}, nil)

	ops.EXPECT().LookupUser("runner").Return(runner, true, nil)
	ops.EXPECT().SubIDRanges(runner).Return(true, false, nil)
	ops.EXPECT().LingerEnabled(runner).Return(false, nil)
	ops.EXPECT().UserManagerRunning(runner).Return(false, machine.ErrUnprivileged)
	for _, p := range []string{".config/picolet/secrets", ".local/share/picolet", ".config/containers/systemd", ".config/systemd/user"} {
		ops.EXPECT().Stat("/home/runner/"+p).Return(machine.PathInfo{}, machine.ErrUnprivileged)
	}

	ops.EXPECT().SystemUnitEnabled(mock.Anything, "podman.socket").Return(true, nil)
	ops.EXPECT().Stat("/etc/picolet/secrets").Return(dir(0, 0, 0o700), nil)
	ops.EXPECT().Stat("/var/lib/picolet-system").Return(dir(0, 0, 0o700), nil)
	ops.EXPECT().Stat("/etc/containers/systemd").Return(dir(0, 0, 0o755), nil)
	ops.EXPECT().Stat("/etc/systemd/system").Return(dir(0, 0, 0o755), nil)
	ops.EXPECT().WorldReadableTree(exampleFleetAbs(t)).Return(true, nil).Times(2)

	out, err := showPlan(t, exampleFleet, ops)
	require.NoError(t, err)
	goldie.New(t).Assert(t, "partial-machine-unprivileged", []byte(out))
}

func TestShowPlanRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  machine.PlanConfig
		want string
	}{
		{
			name: "unknown Machine",
			cfg:  machine.PlanConfig{Machine: "vps-2", RepoDir: exampleFleet, Env: linuxHost()},
			want: `no Host in the Fleet runs on machine "vps-2" (machines: dev-host, e2e-host, node-1, node-2, vps-1, web-1)`,
		},
		{
			name: "missing machine name",
			cfg:  machine.PlanConfig{RepoDir: exampleFleet, Env: linuxHost()},
			want: "machine name is required",
		},
		{
			name: "missing repo dir",
			cfg:  machine.PlanConfig{Machine: "vps-1", Env: linuxHost()},
			want: "--repo-dir is required",
		},
		{
			name: "not Linux",
			cfg:  machine.PlanConfig{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{GOOS: "darwin"}},
			want: "bootstrap machine needs Linux",
		},
		{
			name: "inside a container",
			cfg:  machine.PlanConfig{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{GOOS: "linux", InContainer: true}},
			want: "not inside a container",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			tt.cfg.Stdout = &out
			err := machine.ShowPlan(context.Background(), tt.cfg, mocks.NewMockHostOps(t))
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out.String(), "a rejected plan prints nothing")
		})
	}
}
