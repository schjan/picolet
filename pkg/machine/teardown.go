package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/schjan/picolet/pkg/bootstrap"
	"github.com/schjan/picolet/pkg/config"
)

// TeardownConfig is the input of Teardown.
type TeardownConfig struct {
	Machine string
	// RepoDir is the Fleet checkout on the Machine, which names its Hosts.
	RepoDir string
	Env     Environment
	Stdout  io.Writer
}

// teardown outcomes of a Host.
const (
	tornDown        = "torn down"
	teardownFailed  = "failed"
	teardownSkipped = "skipped"
)

// teardownLeftOver is what Teardown never removes.
const teardownLeftOver = "Left in place: users, subuid/subgid ranges, lingering, podman.socket, " +
	"the Hosts' directories and credential files."

// Teardown tears down every Host the Fleet declares on cfg.Machine, as root:
// each Host's `picolet bootstrap teardown` runs in the per-Host bootstrap's
// container, as the Host's user or as root, and removes everything the
// Host's Agent manages, the Agent included, and its state. Teardown never
// deletes users, subordinate ID ranges or lingering, and leaves
// podman.socket, the Hosts' directories and credential files in place. A
// Host whose user does not exist has nothing to tear down and is skipped. A
// failing Host does not stop the others; Teardown returns the error of every
// failed Host.
func Teardown(ctx context.Context, cfg TeardownConfig, ops HostOps) error {
	if err := cfg.Env.checkRoot("bootstrap teardown --machine", "it runs each Host's teardown as the Host's user"); err != nil {
		return err
	}
	repo, _, err := openCheckout(cfg.RepoDir)
	if err != nil {
		return err
	}
	defer repo.Close()
	hosts, err := machineHosts(repo.Config, cfg.Machine)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(cfg.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Tearing down Machine %s: %d Hosts\n", cfg.Machine, len(hosts))
	counts := map[string]int{}
	var errs []error
	for _, h := range hosts {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("interrupted before %s: %w", h.Hostname, ctx.Err()))
			break
		}
		outcome, detail, err := teardownHost(ctx, ops, repo.Config, h)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h.Hostname, err))
		}
		counts[outcome]++
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", outcome, h.Hostname, detail)
	}
	if err := tw.Flush(); err != nil {
		return errors.Join(append(errs, fmt.Errorf("writing teardown: %w", err))...)
	}
	var summary []string
	for _, o := range []string{tornDown, teardownFailed, teardownSkipped} {
		if counts[o] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", counts[o], o))
		}
	}
	fmt.Fprintf(cfg.Stdout, "\n%s\n%s\n", strings.Join(summary, ", "), teardownLeftOver)
	return errors.Join(errs...)
}

// teardownHost runs h's per-Host teardown and returns its outcome and the
// detail the operator sees; err is a failed teardown's.
func teardownHost(ctx context.Context, ops HostOps, fleet *config.Config, h Host) (outcome, detail string, err error) {
	skip, err := runTeardown(ctx, ops, fleet, h)
	switch {
	case err != nil:
		return teardownFailed, "as " + h.owner() + ": " + err.Error(), err
	case skip != "":
		return teardownSkipped, skip, nil
	}
	return tornDown, "as " + h.owner(), nil
}

// runTeardown runs h's per-Host teardown as its user, or root. skip says
// why there was nothing to tear down. The Agent is named, not rendered: the
// teardown needs no assigned or renderable Agent bundle.
func runTeardown(ctx context.Context, ops HostOps, fleet *config.Config, h Host) (skip string, err error) {
	host, ok := fleet.FindHost(h.Hostname)
	if !ok {
		return "", fmt.Errorf("host not found: %s", h.Hostname)
	}
	agent, err := bootstrap.FleetAgent(fleet, host)
	if err != nil {
		return "", err
	}
	if h.Rootful() {
		return "", ops.RunAsRoot(ctx, hostTeardownCommand(h, User{}, agent))
	}
	user, found, err := ops.LookupUser(h.User)
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf("Linux user %s does not exist: nothing to tear down", h.User), nil
	}
	return "", ops.RunAsUser(ctx, user, hostTeardownCommand(h, user, agent))
}
