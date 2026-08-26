package config

import (
	"errors"

	"go.yaml.in/yaml/v4"
)

// FleetConfig holds fleet-wide configuration from fleet.yml.
type FleetConfig struct {
	Images map[string]string `yaml:"images"`
	Ports  map[string]int    `yaml:"ports"`

	// RetiredPrometheus captures the removed `prometheus:` key so Validate can
	// say so explicitly. The key was never read by picolet.
	// Reject-only — see keyPresent.
	RetiredPrometheus yaml.Node `yaml:"prometheus"`
}

// Validate rejects retired keys.
func (f *FleetConfig) Validate() error {
	if keyPresent(f.RetiredPrometheus) {
		return errors.New(migratePrometheus)
	}
	return nil
}
