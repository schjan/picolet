package config

import (
	"errors"

	"go.yaml.in/yaml/v4"
)

// HostConfig holds per-host configuration from hosts/<hostname>/host.yml.
type HostConfig struct {
	Hostname         string   `yaml:"hostname"`
	ExternalHostname string   `yaml:"external_hostname"`
	Role             string   `yaml:"role"`
	Features         []string `yaml:"features"`

	// DeprecatedPiType captures the pre-rename `pi_type:` key so Validate can
	// reject it with a migration message instead of the generic unknown-field
	// error WithKnownFields() would produce. Reject-only — see keyPresent.
	DeprecatedPiType yaml.Node `yaml:"pi_type"`
}

// Validate checks that required fields are present and that no renamed key is used.
func (h *HostConfig) Validate() error {
	if keyPresent(h.DeprecatedPiType) {
		return errors.New(migratePiType)
	}
	if h.Hostname == "" {
		return errors.New("hostname is required")
	}
	if h.Role == "" {
		return errors.New("role is required")
	}
	return nil
}
