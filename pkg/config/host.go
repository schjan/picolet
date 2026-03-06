package config

import "errors"

// HostConfig holds per-host configuration from hosts/<hostname>/host.yml.
type HostConfig struct {
	Hostname         string   `yaml:"hostname"`
	ExternalHostname string   `yaml:"external_hostname"`
	PiType           string   `yaml:"pi_type"`
	Features         []string `yaml:"features"`
}

// Validate checks that required fields are present.
func (h *HostConfig) Validate() error {
	if h.Hostname == "" {
		return errors.New("hostname is required")
	}
	if h.PiType == "" {
		return errors.New("pi_type is required")
	}
	return nil
}
