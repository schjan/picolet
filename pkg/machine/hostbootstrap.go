package machine

import (
	"context"
	"path"

	"github.com/schjan/picolet/pkg/bootstrap"
)

// repoMount is where the per-Host bootstrap container sees the Fleet
// checkout.
const repoMount = "/repo"

// hostBootstrapCommand is the per-Host bootstrap of h: the Agent image run
// by the Host's own Podman with the Agent quadlet's bind mounts, so the
// container sees every path where the Agent will see it and the state it
// seeds is keyed by those paths. Running it natively as the user would seed
// keys below the home, and the Agent's first Reconciliation would recreate
// and delete its own quadlet. user is the Host's user; ignored for the
// rootful Host.
func hostBootstrapCommand(repoDir string, h Host, user User, agent bootstrap.Agent) Command {
	args := []string{"podman", "run", "--rm", "--network", "host", "-v", repoDir + ":" + repoMount + ":ro"}
	base := "/"
	if !h.Rootful() {
		base = user.Home
	}
	for _, d := range agentDirs {
		args = append(args, "-v", path.Join(base, d.path(h))+":"+d.mount)
	}
	var cmd Command
	systemd := bootstrap.SystemdSystem
	if h.Rootful() {
		args = append(args,
			"-v", "/run/dbus/system_bus_socket:/run/dbus/system_bus_socket",
			"-v", "/run/podman/podman.sock:/run/podman/podman.sock",
			"--security-opt", "apparmor=unconfined")
	} else {
		runtimeDir := user.runtimeDir()
		args = append(args,
			"-v", runtimeDir+"/systemd:"+runtimeDir+"/systemd",
			"-v", runtimeDir+"/podman/podman.sock:/run/podman/podman.sock",
			"-e", "XDG_RUNTIME_DIR="+runtimeDir)
		systemd = bootstrap.SystemdUser
		cmd.Env = []string{"XDG_RUNTIME_DIR=" + runtimeDir}
		cmd.Dir = user.Home
	}
	// --skip-health-wait: the inner wait would time out on an old Agent
	// before bootstrapHost restarts it with the credentials this run wrote;
	// bootstrapHost waits for health itself, after the restart.
	args = append(args, agent.Image, "bootstrap",
		"--hostname", h.Hostname, "--repo-dir", repoMount, "--service", agent.Service, "--systemd", systemd,
		"--skip-health-wait")
	cmd.Args = args
	return cmd
}

// bootstrapHost runs the per-Host bootstrap of s's Host, restarts its Agent
// when this run wrote a credential file of it, and waits for the Agent's
// health at the address its Fleet-rendered config listens on.
func (r *runner) bootstrapHost(ctx context.Context, f *hostFacts, s Step) (string, error) {
	h := s.Host
	agent, err := bootstrap.ResolveAgent(ctx, r.plan.RepoDir, h.Hostname)
	if err != nil {
		return "", err
	}
	report := r.agent(h.Hostname)
	report.addr = agent.DialAddr
	if err := r.startAgent(ctx, f, h, agent, report.restartRequired); err != nil {
		return "", err
	}
	detail := ""
	if report.restartRequired {
		report.restarted = true
		detail = agent.Unit + " restarted (credential files written), "
	}
	if err := r.ops.WaitHealthy(ctx, agent.DialAddr); err != nil {
		report.health = healthUnhealthy
		return "", err
	}
	report.health = healthHealthy
	return detail + "healthy on " + agent.DialAddr, nil
}

// startAgent runs the per-Host bootstrap as the Host's user, or root, and
// then restarts the Agent if restart says so: the bootstrap leaves a running
// Agent with unchanged files alone, so it would keep the old credentials.
func (r *runner) startAgent(ctx context.Context, f *hostFacts, h Host, agent bootstrap.Agent, restart bool) error {
	if h.Rootful() {
		if err := r.ops.RunAsRoot(ctx, hostBootstrapCommand(r.plan.RepoDir, h, User{}, agent)); err != nil {
			return err
		}
		if restart {
			return r.ops.RestartSystemUnit(ctx, agent.Unit)
		}
		return nil
	}
	user, err := f.existingUser()
	if err != nil {
		return err
	}
	if err := r.ops.RunAsUser(ctx, user, hostBootstrapCommand(r.plan.RepoDir, h, user, agent)); err != nil {
		return err
	}
	if restart {
		return r.ops.RestartUserUnit(ctx, user, agent.Unit)
	}
	return nil
}
