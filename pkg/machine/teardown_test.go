package machine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

// userTeardown is the containerized per-Host teardown of a rootless Host:
// the per-Host bootstrap's container, running `bootstrap teardown` instead,
// without the Fleet checkout.
func userTeardown(u machine.User, hostname string) machine.Command {
	run := fmt.Sprintf("/run/user/%d", u.UID)
	return machine.Command{
		Args: strings.Fields("podman run --rm --network host" +
			" -v " + u.Home + "/.config/picolet/secrets:/etc/picolet/secrets:ro" +
			" -v " + u.Home + "/.local/share/picolet:/var/lib/picolet" +
			" -v " + u.Home + "/.config/containers/systemd:/etc/containers/systemd" +
			" -v " + u.Home + "/.config/systemd/user:/etc/systemd/system" +
			" -v " + run + "/systemd:" + run + "/systemd" +
			" -v " + run + "/podman/podman.sock:/run/podman/podman.sock" +
			" -e XDG_RUNTIME_DIR=" + run +
			" " + agentImage + " bootstrap teardown --hostname " + hostname + " --service picolet --systemd user"),
		Env: []string{"XDG_RUNTIME_DIR=" + run},
		Dir: u.Home,
	}
}

// rootTeardown is the containerized per-Host teardown of the rootful Host.
func rootTeardown() machine.Command {
	return machine.Command{
		Args: strings.Fields("podman run --rm --network host" +
			" -v /etc/picolet/secrets:/etc/picolet/secrets:ro" +
			" -v /var/lib/picolet-system:/var/lib/picolet" +
			" -v /etc/containers/systemd:/etc/containers/systemd" +
			" -v /etc/systemd/system:/etc/systemd/system" +
			" -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket" +
			" -v /run/podman/podman.sock:/run/podman/podman.sock" +
			" --security-opt apparmor=unconfined" +
			" " + agentImage + " bootstrap teardown --hostname vps-1-system --service picolet-system --systemd system"),
	}
}

func teardownMachine(t *testing.T, ops machine.HostOps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := machine.Teardown(context.Background(), machine.TeardownConfig{
		Machine: "vps-1",
		RepoDir: exampleFleet,
		Env:     rootOnLinux(),
		Stdout:  &out,
	}, ops)
	return out.String(), err
}

// Teardown runs every Host's teardown in a container of the Host's own
// Podman, as its user or as root. Nothing else of the Machine is touched:
// the mock has no expectation for deleting users, subordinate ID ranges,
// lingering, sockets or directories, so any such call fails the test.
func TestTeardownRunsPerHostTeardownAsEachUser(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	ops.EXPECT().LookupUser("pi").Return(pi, true, nil)
	ops.EXPECT().LookupUser("runner").Return(runner, true, nil)
	ops.EXPECT().RunAsUser(mock.Anything, pi, userTeardown(pi, "vps-1")).Return(nil).Once()
	ops.EXPECT().RunAsUser(mock.Anything, runner, userTeardown(runner, "vps-1-runner")).Return(nil).Once()
	ops.EXPECT().RunAsRoot(mock.Anything, rootTeardown()).Return(nil).Once()

	out, err := teardownMachine(t, ops)
	require.NoError(t, err)
	require.Regexp(t, `\n  torn down\s+vps-1\s+as pi\n`, out)
	require.Regexp(t, `\n  torn down\s+vps-1-runner\s+as runner\n`, out)
	require.Regexp(t, `\n  torn down\s+vps-1-system\s+as root\n`, out)
}

// Teardown needs no renderable Agent bundle: the per-Host teardown removes
// what the Agent's state records. A Host whose Agent was already dropped
// from the Fleet (its picolet service unassigned) is torn down like any
// other, with the conventional service of its systemd instance.
func TestTeardownHostWithoutAssignedAgent(t *testing.T) {
	t.Parallel()
	fleet := t.TempDir()
	require.NoError(t, os.CopyFS(fleet, os.DirFS(exampleFleet)))
	root, err := os.OpenRoot(fleet)
	require.NoError(t, err)
	defer root.Close()
	const host = "hosts/vps-1/host.yml"
	data, err := root.ReadFile(host)
	require.NoError(t, err)
	data = []byte(strings.ReplaceAll(string(data), "features: [agent]", "features: []"))
	require.NoError(t, root.WriteFile(host, data, 0o600))

	ops := mocks.NewMockHostOps(t)
	ops.EXPECT().LookupUser("pi").Return(pi, true, nil)
	ops.EXPECT().LookupUser("runner").Return(runner, true, nil)
	ops.EXPECT().RunAsUser(mock.Anything, pi, userTeardown(pi, "vps-1")).Return(nil).Once()
	ops.EXPECT().RunAsUser(mock.Anything, runner, userTeardown(runner, "vps-1-runner")).Return(nil).Once()
	ops.EXPECT().RunAsRoot(mock.Anything, rootTeardown()).Return(nil).Once()

	var out bytes.Buffer
	err = machine.Teardown(context.Background(), machine.TeardownConfig{
		Machine: "vps-1", RepoDir: fleet, Env: rootOnLinux(), Stdout: &out,
	}, ops)
	require.NoError(t, err)
}

// A Host whose teardown fails does not stop the others, and a Host whose
// user does not exist (never bootstrapped, or already removed by the
// operator) has nothing to tear down. The command fails with the failed
// Host's error.
func TestTeardownFailingHostFailsAlone(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	ops.EXPECT().LookupUser("pi").Return(pi, true, nil)
	ops.EXPECT().LookupUser("runner").Return(machine.User{}, false, nil)
	ops.EXPECT().RunAsUser(mock.Anything, pi, userTeardown(pi, "vps-1")).
		Return(errors.New("podman run: exit status 1: loading state: unexpected end of JSON input")).Once()
	ops.EXPECT().RunAsRoot(mock.Anything, rootTeardown()).Return(nil).Once()

	out, err := teardownMachine(t, ops)
	require.EqualError(t, err, "vps-1: podman run: exit status 1: loading state: unexpected end of JSON input")
	require.Regexp(t, `\n  failed\s+vps-1\s+as pi: podman run: exit status 1`, out)
	require.Regexp(t, `\n  skipped\s+vps-1-runner\s+Linux user runner does not exist: nothing to tear down\n`, out)
	require.Regexp(t, `\n  torn down\s+vps-1-system\s+as root\n`, out)
	require.Contains(t, out, "\n1 torn down, 1 failed, 1 skipped\n")
}

// A teardown that cannot succeed stops before the first Host: the mock has
// no expectations, so any call fails the test.
func TestTeardownFailsFast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  machine.TeardownConfig
		want string
	}{
		{
			name: "not root",
			cfg:  machine.TeardownConfig{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{GOOS: "linux", Podman: true}},
			want: "bootstrap teardown --machine must run as root",
		},
		{
			name: "inside a container",
			cfg: machine.TeardownConfig{Machine: "vps-1", RepoDir: exampleFleet, Env: machine.Environment{
				GOOS: "linux", Root: true, Podman: true, InContainer: true,
			}},
			want: "bootstrap teardown --machine must run on the Machine itself",
		},
		{
			name: "unknown Machine",
			cfg:  machine.TeardownConfig{Machine: "vps-2", RepoDir: exampleFleet, Env: rootOnLinux()},
			want: `no Host in the Fleet runs on machine "vps-2"`,
		},
		{
			name: "missing repo dir",
			cfg:  machine.TeardownConfig{Machine: "vps-1", Env: rootOnLinux()},
			want: "--repo-dir is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			tt.cfg.Stdout = &out
			err := machine.Teardown(context.Background(), tt.cfg, mocks.NewMockHostOps(t))
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out.String(), "a rejected teardown prints nothing")
		})
	}
}
