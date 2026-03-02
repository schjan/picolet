package config

// HostConfig holds per-host configuration from hosts/<hostname>/host.yml.
type HostConfig struct {
	Hostname    string                `yaml:"hostname"`
	AnsibleHost string                `yaml:"ansible_host"`
	PiType      string                `yaml:"pi_type"`
	Features    []string              `yaml:"features"`
	Secrets     map[string]SecretSpec `yaml:"secrets"`
}

// SecretSpec describes a secret file reference.
type SecretSpec struct {
	Path         string `yaml:"path"`
	SkipIfExists bool   `yaml:"skip_if_exists"`
}
