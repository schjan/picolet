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
}

// FleetTemplateData holds fleet-wide template variables.
type FleetTemplateData struct {
	Config *config.FleetConfig
	Hosts  []HostTemplateData
}

// NewTemplateData builds template data for a specific host.
func NewTemplateData(cfg *config.Config, hostname string) (*TemplateData, error) {
	host, ok := cfg.Hosts[hostname]
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}

	allHosts := make([]HostTemplateData, 0, len(cfg.Hosts))
	for _, name := range cfg.SortedHostnames() {
		allHosts = append(allHosts, buildHostData(cfg.Hosts[name]))
	}

	hostData := buildHostData(host)
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
