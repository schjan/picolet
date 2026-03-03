package config

// FleetConfig holds fleet-wide configuration from fleet.yml.
type FleetConfig struct {
	Images     map[string]string `yaml:"images"`
	Ports      map[string]int    `yaml:"ports"`
	Prometheus map[string]any    `yaml:"prometheus"`
}
