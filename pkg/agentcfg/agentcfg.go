package agentcfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	op "github.com/schjan/picolet/pkg/onepassword"
)

// MQTTConfig holds MQTT broker connection settings.
type MQTTConfig struct {
	BrokerURL    string `yaml:"broker_url"` // required: tcp://host:1883 or ssl://host:8883
	Username     string `yaml:"username"`
	PasswordPath string `yaml:"password_path"` // file path to password, not inline
	TopicPrefix  string `yaml:"topic_prefix"`  // default: "picolet"
}

// OnePasswordConfig holds 1Password SDK settings.
type OnePasswordConfig struct {
	TokenPath             string        `yaml:"token_path"`                 // file path to service account token
	RefreshInterval       time.Duration `yaml:"refresh_interval"`           // how often to re-fetch op:// secrets (default 6h)
	GitTokenRef           string        `yaml:"git_token_ref"`              // op:// ref for git pull token; replaces git_token_path
	GitHubAppIDRef        string        `yaml:"github_app_id_ref"`          // op:// ref for GitHub App ID
	GitHubInstallationRef string        `yaml:"github_installation_id_ref"` // op:// ref for GitHub installation ID
	GitHubPrivateKeyRef   string        `yaml:"github_private_key_ref"`     // op:// ref for GitHub App PEM private key
}

// Config holds the agent runtime configuration from /etc/picolet/config.yml.
type Config struct {
	Hostname             string             `yaml:"hostname"`
	RepoURL              string             `yaml:"repo_url"`
	RepoBranch           string             `yaml:"repo_branch"`
	GitTokenPath         string             `yaml:"git_token_path"`
	PollInterval         time.Duration      `yaml:"poll_interval"`
	MetricsPort          int                `yaml:"metrics_port"`
	SecretsDir           string             `yaml:"secrets_dir"`
	Rootless             bool               `yaml:"rootless"`
	SystemdUser          *bool              `yaml:"systemd_user"`
	PodmanSocket         string             `yaml:"podman_socket"`
	WebhookSecretPath    string             `yaml:"webhook_secret_path"`
	RepoSubDir           string             `yaml:"repo_sub_dir"` // optional subdirectory within the repo to use as fleet root (monorepo support)
	DataDir              string             `yaml:"data_dir"`     // optional override for state file directory; used by apply/down commands
	MQTT                 *MQTTConfig        `yaml:"mqtt"`
	OnePassword          *OnePasswordConfig `yaml:"onepassword"`
	GitHubAppID          int64              `yaml:"github_app_id"`
	GitHubInstallationID int64              `yaml:"github_installation_id"`
	GitHubPrivateKeyPath string             `yaml:"github_private_key_path"`
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

//nolint:cyclop // sequential field defaults; splitting would obscure the logic
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
	if c.OnePassword != nil && c.OnePassword.RefreshInterval == 0 {
		c.OnePassword.RefreshInterval = 6 * time.Hour
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
//
//nolint:cyclop // sequential field checks; splitting would obscure the validation logic
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
	if c.OnePassword != nil {
		if err := c.validateOnePassword(); err != nil {
			return err
		}
	}
	if err := c.validateGitHubApp(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateOnePassword() error {
	if c.OnePassword.TokenPath == "" {
		return errors.New("onepassword.token_path is required when onepassword is configured")
	}
	if c.OnePassword.RefreshInterval < time.Minute {
		return errors.New("onepassword.refresh_interval must be at least 1m")
	}
	if err := validateOptionalOpRef("onepassword.git_token_ref", c.OnePassword.GitTokenRef); err != nil {
		return err
	}
	if c.OnePassword.GitTokenRef != "" {
		if c.GitTokenPath != "" {
			return errors.New("git_token_path and onepassword.git_token_ref are mutually exclusive")
		}
	}
	for key, ref := range map[string]string{
		"onepassword.github_app_id_ref":          c.OnePassword.GitHubAppIDRef,
		"onepassword.github_installation_id_ref": c.OnePassword.GitHubInstallationRef,
		"onepassword.github_private_key_ref":     c.OnePassword.GitHubPrivateKeyRef,
	} {
		if err := validateOptionalOpRef(key, ref); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateGitHubApp() error {
	directSet := countSet(c.GitHubAppID != 0, c.GitHubInstallationID != 0, c.GitHubPrivateKeyPath != "")
	refSet := 0
	if c.OnePassword != nil {
		refSet = countSet(
			c.OnePassword.GitHubAppIDRef != "",
			c.OnePassword.GitHubInstallationRef != "",
			c.OnePassword.GitHubPrivateKeyRef != "",
		)
	}

	if err := validateGitHubAppMode(directSet, refSet); err != nil {
		return err
	}
	if directSet == 0 && refSet == 0 {
		return nil
	}
	if c.GitTokenPath != "" {
		return errors.New("github_app_id and git_token_path are mutually exclusive")
	}
	if c.OnePassword != nil && c.OnePassword.GitTokenRef != "" {
		return errors.New("github_app_id and onepassword.git_token_ref are mutually exclusive")
	}
	if directSet == 0 {
		return nil
	}
	return validateGitHubAppDirectValues(c.GitHubAppID, c.GitHubInstallationID)
}

// HasGitHubApp reports whether GitHub App authentication is configured.
func (c *Config) HasGitHubApp() bool {
	return (c.GitHubAppID != 0 && c.GitHubInstallationID != 0 && c.GitHubPrivateKeyPath != "") || c.HasGitHubAppRefs()
}

// HasGitHubAppRefs reports whether GitHub App credentials are configured via 1Password refs.
func (c *Config) HasGitHubAppRefs() bool {
	if c.OnePassword == nil {
		return false
	}
	return c.OnePassword.GitHubAppIDRef != "" && c.OnePassword.GitHubInstallationRef != "" && c.OnePassword.GitHubPrivateKeyRef != ""
}

func validateOptionalOpRef(name, ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := op.ParseOpRef(ref); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func countSet(values ...bool) int {
	var set int
	for _, v := range values {
		if v {
			set++
		}
	}
	return set
}

func validateGitHubAppMode(directSet, refSet int) error {
	switch {
	case directSet > 0 && refSet > 0:
		return errors.New("github app direct fields and onepassword github app refs are mutually exclusive")
	case directSet > 0 && directSet < 3:
		return errors.New("all GitHub App fields must be set together (github_app_id, github_installation_id, github_private_key_path)")
	case refSet > 0 && refSet < 3:
		return errors.New("all onepassword github app refs must be set together (onepassword.github_app_id_ref, onepassword.github_installation_id_ref, onepassword.github_private_key_ref)")
	default:
		return nil
	}
}

func validateGitHubAppDirectValues(appID, installationID int64) error {
	if appID <= 0 {
		return errors.New("github_app_id must be positive")
	}
	if installationID <= 0 {
		return errors.New("github_installation_id must be positive")
	}
	return nil
}
