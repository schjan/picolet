package bootstrap

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// Agent is a Host's Agent as the Fleet renders it: what `bootstrap machine`
// needs to start it through the per-Host bootstrap and to probe it.
type Agent struct {
	// Service is the Agent's Service Bundle: picolet for a Host with user:,
	// picolet-system for the rootful Host.
	Service string
	// Unit is the systemd unit the bundle's quadlet generates.
	Unit string
	// Image is fleet.yml images.picolet, which the per-Host bootstrap runs.
	Image string
	// DialAddr is where a client on the Machine reaches the Agent: its
	// rendered listen address, a wildcard host replaced by loopback.
	DialAddr string
}

// ResolveAgent resolves the Agent of hostname from the Fleet checked out at
// repoDir, as the per-Host bootstrap in the Agent's container resolves it.
// Credential files are not read: the Agent config needs none to say where the
// Agent listens.
func ResolveAgent(ctx context.Context, repoDir, hostname string) (Agent, error) {
	repo, err := openRepo(repoDir)
	if err != nil {
		return Agent{}, err
	}
	defer repo.Close()
	host, ok := repo.Config.FindHost(hostname)
	if !ok {
		return Agent{}, fmt.Errorf("host not found: %s", hostname)
	}
	// The container's layout: no --rootless, no --data-dir, and the default
	// service of the Host's systemd instance.
	mode := SystemdUser
	if host.Rootful() {
		mode = SystemdSystem
	}
	tgt, err := Target{SystemdMode: mode}.resolve()
	if err != nil {
		return Agent{}, err
	}
	if assigned := repo.Config.Assignments.Resolve(host).Services; !slices.Contains(assigned, tgt.service) {
		return Agent{}, fmt.Errorf("host %s has no %s service assigned, the Agent of a %s (assigned: %s)",
			hostname, tgt.service, agentKind(host), strings.Join(assigned, ", "))
	}
	image := repo.Config.Fleet.Images["picolet"]
	if image == "" {
		return Agent{}, fmt.Errorf("fleet.yml images has no picolet key: the image the per-Host bootstrap of %s runs", hostname)
	}

	resolved, err := resolveBootstrapHost(ctx, repo, resolveConfig{
		Hostname:   hostname,
		Service:    tgt.service,
		DataDir:    tgt.dataDir,
		SecretsDir: defaultSecretsDir,
		FileMode:   fileReaderPlaceholder,
	})
	if err != nil {
		return Agent{}, err
	}
	if err := verifyUnitResolved(resolved.Files, tgt); err != nil {
		return Agent{}, err
	}
	addr, err := healthAddrFromResolved(resolved.Files, tgt.unitName)
	if err != nil {
		return Agent{}, err
	}
	return Agent{Service: tgt.service, Unit: tgt.unitName, Image: image, DialAddr: addr}, nil
}

func agentKind(host *config.HostConfig) string {
	if host.Rootful() {
		return "rootful Host"
	}
	return "Host with user:"
}
