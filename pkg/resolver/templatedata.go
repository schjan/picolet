package resolver

import (
	"github.com/schjan/picolet/pkg/config"
)

// TemplateData is the root context passed to all templates as `.`.
type TemplateData struct {
	Host   HostTemplateData
	Fleet  FleetTemplateData
	Images map[string]string
	Ports  map[string]int
}

// HostTemplateData holds per-host template variables.
type HostTemplateData struct {
	Hostname         string
	ExternalHostname string
	PiType           string
	Features         []string

	// Services is the resolved bundle name list for this host, merged from
	// assignments.yml (base + pi_type + features). Sorted and deduplicated.
	// Mirrors Assignments.Resolve(host).Services. Populated only for .Host;
	// .Fleet.Hosts entries leave it empty.
	Services []string

	// SystemdUnits is the sorted, deduplicated list of systemd unit names
	// picolet manages on this host:
	//   - Quadlet-derived units (.container, .kube, .network, .volume) via
	//     Podman's parser, which honors ServiceName= overrides.
	//   - Raw systemd units (CategorySystemd), where the unit name is the
	//     filename with any .tmpl suffix stripped (e.g. "https.socket").
	// Populated by the first render pass — see prepareTemplateData in
	// resolver.go. Populated only for .Host; .Fleet.Hosts entries leave it empty.
	SystemdUnits []string
}

// FleetTemplateData holds fleet-wide template variables.
type FleetTemplateData struct {
	Config *config.FleetConfig
	Hosts  []HostTemplateData
}

// NewTemplateData builds template data for a specific host.
func NewTemplateData(cfg *config.Config, hostname string) (*TemplateData, error) {
	host, ok := cfg.FindHost(hostname)
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}

	allHosts := make([]HostTemplateData, 0, len(cfg.Hosts))
	for _, name := range cfg.SortedHostnames() {
		allHosts = append(allHosts, buildHostData(cfg.Hosts[name]))
	}

	hostData := buildHostData(host)
	// Services is knowable without rendering, so populate it here — it is then
	// available to the first render pass. SystemdUnits needs the first pass and
	// is set later by prepareTemplateData. Assignments.Resolve is a pure
	// in-memory merge; cfg.Assignments is non-nil whenever config.LoadAll succeeds.
	hostData.Services = cfg.Assignments.Resolve(host).Services
	return &TemplateData{
		Host: hostData,
		Fleet: FleetTemplateData{
			Config: cfg.Fleet,
			Hosts:  allHosts,
		},
		Images: cfg.Fleet.Images,
		Ports:  cfg.Fleet.Ports,
	}, nil
}

func buildHostData(host *config.HostConfig) HostTemplateData {
	return HostTemplateData{
		Hostname:         host.Hostname,
		ExternalHostname: host.ExternalHostname,
		PiType:           host.PiType,
		Features:         host.Features,
	}
}

// HostNotFoundError is returned when a hostname is not in the config.
type HostNotFoundError struct {
	Hostname string
}

func (e *HostNotFoundError) Error() string {
	return "host not found: " + e.Hostname
}
