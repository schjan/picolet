package machine

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/schjan/picolet/pkg/bootstrap"
)

// RunAsUser implements HostOps. runuser opens a PAM session of the user, as
// rootless Podman expects; cmd.Env goes through env, since runuser resets
// HOME, USER and LOGNAME but passes the rest of root's environment on.
func (o *OSHostOps) RunAsUser(ctx context.Context, u User, c Command) error {
	if len(c.Args) == 0 {
		return errors.New("run as user: no command")
	}
	args := append([]string{"-u", u.Name, "--", "env"}, c.Env...)
	cmd := exec.CommandContext(ctx, "runuser", append(args, c.Args...)...) //nolint:gosec // the executor's own command
	cmd.Dir = c.Dir
	return run(cmd)
}

// RunAsRoot implements HostOps.
func (o *OSHostOps) RunAsRoot(ctx context.Context, c Command) error {
	if len(c.Args) == 0 {
		return errors.New("run as root: no command")
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...) //nolint:gosec // the executor's own command
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir = c.Dir
	return run(cmd)
}

// RestartUserUnit implements HostOps.
func (o *OSHostOps) RestartUserUnit(ctx context.Context, u User, unit string) error {
	return o.userSystemctl(ctx, u, "restart", unit)
}

// RestartSystemUnit implements HostOps.
func (o *OSHostOps) RestartSystemUnit(ctx context.Context, unit string) error {
	return runCommand(ctx, "systemctl", "restart", unit)
}

// WaitHealthy implements HostOps.
func (o *OSHostOps) WaitHealthy(ctx context.Context, addr string) error {
	return bootstrap.WaitAgentHealthy(ctx, addr)
}
