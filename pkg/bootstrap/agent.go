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
	repo, err := config.OpenRepo(repoDir)
	if err != nil {
		return Agent{}, err
	}
	defer repo.Close()
	host, ok := repo.Config.FindHost(hostname)
	if !ok {
		return Agent{}, fmt.Errorf("host not found: %s", hostname)
	}
	service := defaultService(!host.Rootful())
	if assigned := repo.Config.Assignments.Resolve(host).Services; !slices.Contains(assigned, service) {
		return Agent{}, fmt.Errorf("host %s has no %s service assigned, the Agent of a %s (assigned: %s)",
			hostname, service, agentKind(host), strings.Join(assigned, ", "))
	}
	image := repo.Config.Fleet.Images["picolet"]
	if image == "" {
		return Agent{}, fmt.Errorf("fleet.yml images has no picolet key: the image the per-Host bootstrap of %s runs", hostname)
	}

	// The container's layout: no --rootless, no --data-dir.
	containerDataDir, err := dataDir(false, "")
	if err != nil {
		return Agent{}, err
	}
	tgt := resolvedTarget{service: service, unitName: service + ".service"}
	resolved, err := resolveBootstrapHost(ctx, resolveConfig{
		RepoDir:    repoDir,
		Hostname:   hostname,
		Service:    service,
		DataDir:    containerDataDir,
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
	return Agent{Service: service, Unit: tgt.unitName, Image: image, DialAddr: addr}, nil
}

func agentKind(host *config.HostConfig) string {
	if host.Rootful() {
		return "rootful Host"
	}
	return "Host with user:"
}
