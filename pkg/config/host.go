package config

import "errors"

// HostConfig holds per-host configuration from hosts/<hostname>/host.yml.
type HostConfig struct {
	Hostname         string                `yaml:"hostname"`
	ExternalHostname string                `yaml:"external_hostname"`
	PiType           string                `yaml:"pi_type"`
	Features         []string              `yaml:"features"`
	Secrets          map[string]SecretSpec `yaml:"secrets"`
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

// SecretSpec describes a secret file reference.
type SecretSpec struct {
	Path         string `yaml:"path"`
	SkipIfExists bool   `yaml:"skip_if_exists"`
}
