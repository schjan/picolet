package resolver

import (
	"cmp"
	"slices"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
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
	Role             string
	Features         []string

	// Machine is the Machine the Host runs on (defaults to Hostname).
	Machine string
	// User is the Linux user the Host's Agent runs as; empty when Rootful.
	User string
	// Rootful is true for the Host whose Agent runs as root (no user:).
	Rootful bool
	// ListenPort is the port the Host's Agent listens on.
	ListenPort int
	// AgeRecipients is the Host's age_recipients: in canonical form, in
	// file order; empty when it declares none.
	AgeRecipients []string

	// Services is the resolved bundle name list for this host, merged from
	// assignments.yml (base + role + features). Sorted and deduplicated.
	// Mirrors Assignments.Resolve(host).Services. Populated only for .Host;
	// .Fleet.Hosts entries leave it empty.
	Services []string

	// SystemdUnits is the sorted, deduplicated list of systemd unit names
	// picolet manages on this host:
	//   - Quadlet-derived units (.container, .kube, .network, .volume, .pod,
	//     .image, .build) via Podman's parser, which honors ServiceName=
	//     overrides.
	//   - Raw systemd units (CategorySystemd), where the unit name is the
	//     filename with any .tmpl suffix stripped (e.g. "https.socket").
	// Populated by the first render pass — see prepareTemplateData in
	// resolver.go. Populated only for .Host; .Fleet.Hosts entries leave it empty.
	SystemdUnits []string
}

// FleetTemplateData holds fleet-wide template variables.
type FleetTemplateData struct {
	Config FleetConfigTemplateData
	Hosts  []HostTemplateData
}

// FleetConfigTemplateData is fleet.yml as templates see it (.Fleet.Config):
// its values only, never the shared *config.FleetConfig, whose yaml.Node
// field has methods (Encode, SetString) that would let one render hand a
// value to a later one or to another Host's resolve.
type FleetConfigTemplateData struct {
	Images    map[string]string
	Ports     map[string]int
	Bootstrap config.FleetBootstrap
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
			Config: FleetConfigTemplateData{
				Images:    cfg.Fleet.Images,
				Ports:     cfg.Fleet.Ports,
				Bootstrap: cfg.Fleet.Bootstrap,
			},
			Hosts: allHosts,
		},
		Images: cfg.Fleet.Images,
		Ports:  cfg.Fleet.Ports,
	}, nil
}

func buildHostData(host *config.HostConfig) HostTemplateData {
	return HostTemplateData{
		AgeRecipients:    sops.RecipientStrings(host.AgeRecipients),
		Hostname:         host.Hostname,
		ExternalHostname: host.ExternalHostname,
		Role:             host.Role,
		Features:         host.Features,
		Machine:          host.Machine,
		User:             host.User,
		Rootful:          host.Rootful(),
		ListenPort:       host.ListenPort,
	}
}

// siblings returns the other Hosts on the current Host's Machine (compared by
// config.MachineKey), sorted by hostname. It backs the zero-argument
// `siblings` template helper.
func siblings(data *TemplateData) []HostTemplateData {
	machine := config.MachineKey(data.Host.Machine)
	var out []HostTemplateData
	for _, h := range data.Fleet.Hosts {
		if config.MachineKey(h.Machine) == machine && h.Hostname != data.Host.Hostname {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b HostTemplateData) int { return cmp.Compare(a.Hostname, b.Hostname) })
	return out
}

// HostNotFoundError is returned when a hostname is not in the config.
type HostNotFoundError struct {
	Hostname string
}

func (e *HostNotFoundError) Error() string {
	return "host not found: " + e.Hostname
}
