package agentcfg

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v4"
)

// Config holds the agent runtime configuration from /etc/picolet/config.yml.
type Config struct {
	Hostname     string        `yaml:"hostname"`
	RepoURL      string        `yaml:"repo_url"`
	RepoBranch   string        `yaml:"repo_branch"`
	GitTokenPath string        `yaml:"git_token_path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MetricsPort  int           `yaml:"metrics_port"`
	SecretsDir   string        `yaml:"secrets_dir"`
}

// Load reads and parses the agent config from disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) setDefaults() {
	if c.RepoBranch == "" {
		c.RepoBranch = "main"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 60 * time.Second
	}
	if c.MetricsPort == 0 {
		c.MetricsPort = 9417
	}
	if c.SecretsDir == "" {
		c.SecretsDir = "/etc/picolet/secrets"
	}
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if c.Hostname == "" {
		return errors.New("hostname is required")
	}
	if c.RepoURL == "" {
		return errors.New("repo_url is required")
	}
	return nil
}
