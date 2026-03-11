package agentcfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// MQTTConfig holds MQTT broker connection settings.
type MQTTConfig struct {
	BrokerURL    string `yaml:"broker_url"` // required: tcp://host:1883 or ssl://host:8883
	Username     string `yaml:"username"`
	PasswordPath string `yaml:"password_path"` // file path to password, not inline
	TopicPrefix  string `yaml:"topic_prefix"`  // default: "picolet"
}

// Config holds the agent runtime configuration from /etc/picolet/config.yml.
type Config struct {
	Hostname          string        `yaml:"hostname"`
	RepoURL           string        `yaml:"repo_url"`
	RepoBranch        string        `yaml:"repo_branch"`
	GitTokenPath      string        `yaml:"git_token_path"`
	PollInterval      time.Duration `yaml:"poll_interval"`
	MetricsPort       int           `yaml:"metrics_port"`
	SecretsDir        string        `yaml:"secrets_dir"`
	Rootless          bool          `yaml:"rootless"`
	SystemdUser       *bool         `yaml:"systemd_user"`
	PodmanSocket      string        `yaml:"podman_socket"`
	WebhookSecretPath string        `yaml:"webhook_secret_path"`
	RepoSubDir        string        `yaml:"repo_sub_dir"` // optional subdirectory within the repo to use as fleet root (monorepo support)
	DataDir           string        `yaml:"data_dir"`     // optional override for state file directory; used by apply/down commands
	MQTT              *MQTTConfig   `yaml:"mqtt"`
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
		c.PollInterval = 5 * time.Minute
	}
	if c.MetricsPort == 0 {
		c.MetricsPort = 9417
	}
	if c.SecretsDir == "" {
		c.SecretsDir = "/etc/picolet/secrets"
	}
	if c.PodmanSocket == "" {
		c.PodmanSocket = "/run/podman/podman.sock"
	}
	if c.MQTT != nil && c.MQTT.TopicPrefix == "" {
		c.MQTT.TopicPrefix = "picolet"
	}
	if c.SystemdUser == nil {
		c.SystemdUser = new(c.Rootless)
	}
}

// UseSystemdUser reports whether the agent should connect to the user systemd instance.
// Defaults to Rootless when systemd_user is not set explicitly in config.
func (c *Config) UseSystemdUser() bool {
	if c.SystemdUser == nil {
		return c.Rootless
	}
	return *c.SystemdUser
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if c.Hostname == "" {
		return errors.New("hostname is required")
	}
	if c.RepoSubDir != "" {
		cleaned := filepath.Clean(c.RepoSubDir)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			return fmt.Errorf("repo_sub_dir must be a relative path within the repo: %q", c.RepoSubDir)
		}
	}
	if c.MQTT != nil && c.MQTT.BrokerURL == "" {
		return errors.New("mqtt.broker_url is required when mqtt is configured")
	}
	if c.MQTT != nil && c.MQTT.PasswordPath != "" && c.MQTT.Username == "" {
		return errors.New("mqtt.username is required when mqtt.password_path is set")
	}
	return nil
}
