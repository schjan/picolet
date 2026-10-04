package machine_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

// agentImage is fleet.yml images.picolet of the example fleet.
const agentImage = "ghcr.io/schjan/picolet:v0.2.0"

// userBootstrap is the containerized per-Host bootstrap of a rootless Host:
// the user's own Podman runs the Agent image with the Agent quadlet's bind
// mounts, so the container sees the paths the Agent will see.
func userBootstrap(repo string, u machine.User, hostname string) machine.Command {
	run := fmt.Sprintf("/run/user/%d", u.UID)
	return machine.Command{
		Args: strings.Fields("podman run --rm --network host" +
			" -v " + repo + ":/repo:ro" +
			" -v " + u.Home + "/.config/picolet/secrets:/etc/picolet/secrets:ro" +
			" -v " + u.Home + "/.local/share/picolet:/var/lib/picolet" +
			" -v " + u.Home + "/.config/containers/systemd:/etc/containers/systemd" +
			" -v " + u.Home + "/.config/systemd/user:/etc/systemd/system" +
			" -v " + run + "/systemd:" + run + "/systemd" +
			" -v " + run + "/podman/podman.sock:/run/podman/podman.sock" +
			" -e XDG_RUNTIME_DIR=" + run +
			" " + agentImage + " bootstrap --hostname " + hostname + " --repo-dir /repo --service picolet --systemd user"),
		Env: []string{"XDG_RUNTIME_DIR=" + run},
		Dir: u.Home,
	}
}

// rootBootstrap is the containerized per-Host bootstrap of the rootful Host:
// the system paths, the system bus, and AppArmor unconfined like the Agent
// quadlet, so the container may talk to systemd.
func rootBootstrap(repo string) machine.Command {
	return machine.Command{
		Args: strings.Fields("podman run --rm --network host" +
			" -v " + repo + ":/repo:ro" +
			" -v /etc/picolet/secrets:/etc/picolet/secrets:ro" +
			" -v /var/lib/picolet-system:/var/lib/picolet" +
			" -v /etc/containers/systemd:/etc/containers/systemd" +
			" -v /etc/systemd/system:/etc/systemd/system" +
			" -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket" +
			" -v /run/podman/podman.sock:/run/podman/podman.sock" +
			" --security-opt apparmor=unconfined" +
			" " + agentImage + " bootstrap --hostname vps-1-system --repo-dir /repo --service picolet-system --systemd system"),
	}
}

// expectAgentsStarted sets ops up to start every Agent of vps-1 through its
// containerized per-Host bootstrap, each reporting healthy.
func expectAgentsStarted(t *testing.T, ops *mocks.MockHostOps) {
	t.Helper()
	expectAgentsStartedAt(ops, exampleFleetAbs(t))
}

// expectAgentsStartedAt is expectAgentsStarted for the Fleet checked out at
// repo.
func expectAgentsStartedAt(ops *mocks.MockHostOps, repo string) {
	expectUserAgentStarted(ops, repo, pi, "vps-1", "127.0.0.1:9417")
	expectUserAgentStarted(ops, repo, runner, "vps-1-runner", "127.0.0.1:9419")
	expectRootfulAgentStarted(ops, repo)
}

func expectUserAgentStarted(ops *mocks.MockHostOps, repo string, u machine.User, hostname, addr string) {
	ops.EXPECT().RunAsUser(mock.Anything, u, userBootstrap(repo, u, hostname)).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, addr).Return(nil).Once()
}

func expectRootfulAgentStarted(ops *mocks.MockHostOps, repo string) {
	ops.EXPECT().RunAsRoot(mock.Anything, rootBootstrap(repo)).Return(nil).Once()
	// The rootful Agent binds 0.0.0.0; the probe dials loopback.
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9418").Return(nil).Once()
}

// callLog records, in call order, the HostOps calls that change the
// Machine or wait on it.
type callLog struct{ calls []string }

func (l *callLog) record(call string) func(mock.Arguments) {
	return func(mock.Arguments) { l.calls = append(l.calls, call) }
}

// Phases run Machine-wide in order: every Host is set up, then every
// credential file is placed, before the first per-Host bootstrap. A Host
// whose credential file this run wrote gets its Agent restarted after its
// per-Host bootstrap, which leaves a running Agent with unchanged files
// alone, and before its health wait: the health that counts is the Agent's
// with the new credentials.
func TestRunRestartsAgentsWithNewCredentialsBeforeHealthWait(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	repo := exampleFleetAbs(t)
	var log callLog
	stopped := machine.UnitState{Enabled: true}
	expectBootstrappedUser(ops, pi, stopped)
	ops.EXPECT().EnableUserUnit(mock.Anything, pi, "podman.socket").Call.Run(log.record("socket vps-1")).Return(nil).Once()
	expectBootstrappedUser(ops, runner, running)
	expectBootstrappedRootful(ops, stopped)
	ops.EXPECT().EnableSystemUnit(mock.Anything, "podman.socket").Call.Run(log.record("socket vps-1-system")).Return(nil).Once()
	ops.EXPECT().WorldReadableTree(repo).Return(true, nil)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/git-token", []byte("pi-token"), pi.Owner(), os.FileMode(0o600)).
		Call.Run(log.record("write pi git-token")).Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/git-token", []byte("system-token"), machine.Owner{}, os.FileMode(0o600)).
		Call.Run(log.record("write system git-token")).Return(nil).Once()
	ops.EXPECT().RunAsUser(mock.Anything, pi, userBootstrap(repo, pi, "vps-1")).Call.Run(log.record("bootstrap vps-1")).Return(nil).Once()
	ops.EXPECT().RestartUserUnit(mock.Anything, pi, "picolet.service").Call.Run(log.record("restart vps-1")).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9417").Call.Run(log.record("health vps-1")).Return(nil).Once()
	ops.EXPECT().RunAsUser(mock.Anything, runner, userBootstrap(repo, runner, "vps-1-runner")).
		Call.Run(log.record("bootstrap vps-1-runner")).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9419").Call.Run(log.record("health vps-1-runner")).Return(nil).Once()
	ops.EXPECT().RunAsRoot(mock.Anything, rootBootstrap(repo)).Call.Run(log.record("bootstrap vps-1-system")).Return(nil).Once()
	ops.EXPECT().RestartSystemUnit(mock.Anything, "picolet-system.service").Call.Run(log.record("restart vps-1-system")).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9418").Call.Run(log.record("health vps-1-system")).Return(nil).Once()

	out, err := runWithSecrets(t, secretsDir(t, map[string]string{
		"vps-1/git-token":        "pi-token",
		"vps-1-system/git-token": "system-token",
	}), ops)
	require.NoError(t, err)
	require.Equal(t, []string{
		"socket vps-1", "socket vps-1-system",
		"write pi git-token", "write system git-token",
		"bootstrap vps-1", "restart vps-1", "health vps-1",
		"bootstrap vps-1-runner", "health vps-1-runner",
		"bootstrap vps-1-system", "restart vps-1-system", "health vps-1-system",
	}, log.calls)
	goldie.New(t).Assert(t, "run-restarts-agents", []byte(out))
}

// A Host whose per-Host bootstrap fails, or whose Agent does not become
// healthy, fails alone: the other Hosts' Agents are started, the summary
// lists every Host, and the run fails with each failed Host's error.
func TestRunFailingAgentFailsOnlyItsHost(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	repo := exampleFleetAbs(t)
	ops.EXPECT().RunAsUser(mock.Anything, pi, userBootstrap(repo, pi, "vps-1")).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9417").Return(errors.New("picolet did not report healthy within 1m30s")).Once()
	ops.EXPECT().RunAsUser(mock.Anything, runner, userBootstrap(repo, runner, "vps-1-runner")).
		Return(errors.New("podman run: exit status 125: image not known")).Once()
	ops.EXPECT().RunAsRoot(mock.Anything, rootBootstrap(repo)).Return(nil).Once()
	ops.EXPECT().WaitHealthy(mock.Anything, "127.0.0.1:9418").Return(nil).Once()

	out, err := runMachine(t, ops)
	require.EqualError(t, err, "vps-1/bootstrap: picolet did not report healthy within 1m30s\n"+
		"vps-1-runner/bootstrap: podman run: exit status 125: image not known")
	goldie.New(t).Assert(t, "run-failing-agent", []byte(out))
}
