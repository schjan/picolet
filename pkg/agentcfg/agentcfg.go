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
	pp "github.com/schjan/picolet/pkg/protonpass"
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

// ProtonPassConfig holds Proton Pass CLI settings.
//
// PATPath enables non-interactive auto-login (containers/CI). When empty,
// the agent uses any pre-existing pass-cli session — useful for local
// development where overwriting the user's session is undesirable.
//
// EncryptionKeyPath is mandatory when PATPath is set; the contents seed
// pass-cli's env-mode key provider.
type ProtonPassConfig struct {
	CLIPath               string        `yaml:"cli_path"`                   // optional; default "pass-cli"
	PATPath               string        `yaml:"pat_path"`                   // optional; empty = lazy mode
	EncryptionKeyPath     string        `yaml:"encryption_key_path"`        // required when pat_path is set
	SessionDir            string        `yaml:"session_dir"`                // optional; default /var/lib/picolet/protonpass/.session
	RefreshInterval       time.Duration `yaml:"refresh_interval"`           // how often to re-fetch pass:// secrets (default 6h)
	GitTokenRef           string        `yaml:"git_token_ref"`              // pass:// ref for git pull token
	GitHubAppIDRef        string        `yaml:"github_app_id_ref"`          // pass:// ref for GitHub App ID
	GitHubInstallationRef string        `yaml:"github_installation_id_ref"` // pass:// ref for GitHub installation ID
	GitHubPrivateKeyRef   string        `yaml:"github_private_key_ref"`     // pass:// ref for GitHub App PEM private key
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
	DataDir              string             `yaml:"data_dir"`     // optional override for repo, state, and lock files
	MQTT                 *MQTTConfig        `yaml:"mqtt"`
	OnePassword          *OnePasswordConfig `yaml:"onepassword"`
	ProtonPass           *ProtonPassConfig  `yaml:"protonpass"`
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
	if err := yaml.Load(data, &cfg, yaml.WithKnownFields()); err != nil {
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
	if c.ProtonPass != nil && c.ProtonPass.RefreshInterval == 0 {
		c.ProtonPass.RefreshInterval = 6 * time.Hour
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
	if c.ProtonPass != nil {
		if err := c.validateProtonPass(); err != nil {
			return err
		}
	}
	if err := c.validateGitTokenSources(); err != nil {
		return err
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

func (c *Config) validateProtonPass() error {
	if c.ProtonPass.PATPath != "" && c.ProtonPass.EncryptionKeyPath == "" {
		return errors.New("protonpass.encryption_key_path is required when pat_path is set")
	}
	if c.ProtonPass.RefreshInterval < time.Minute {
		return errors.New("protonpass.refresh_interval must be at least 1m")
	}
	for key, ref := range map[string]string{
		"protonpass.git_token_ref":               c.ProtonPass.GitTokenRef,
		"protonpass.github_app_id_ref":           c.ProtonPass.GitHubAppIDRef,
		"protonpass.github_installation_id_ref":  c.ProtonPass.GitHubInstallationRef,
		"protonpass.github_private_key_ref":      c.ProtonPass.GitHubPrivateKeyRef,
	} {
		if err := validateOptionalPassRef(key, ref); err != nil {
			return err
		}
	}
	return nil
}

// validateGitTokenSources enforces that at most one source is configured for
// the git pull token: file (git_token_path), 1Password ref, or Proton Pass ref.
//
// Existing OP-only error message is preserved for backward compatibility with
// tests; the PP variants use parallel wording.
func (c *Config) validateGitTokenSources() error {
	opRef := c.OnePassword != nil && c.OnePassword.GitTokenRef != ""
	ppRef := c.ProtonPass != nil && c.ProtonPass.GitTokenRef != ""
	switch {
	case c.GitTokenPath != "" && opRef:
		return errors.New("git_token_path and onepassword.git_token_ref are mutually exclusive")
	case c.GitTokenPath != "" && ppRef:
		return errors.New("git_token_path and protonpass.git_token_ref are mutually exclusive")
	case opRef && ppRef:
		return errors.New("onepassword.git_token_ref and protonpass.git_token_ref are mutually exclusive")
	}
	return nil
}

func (c *Config) validateGitHubApp() error {
	directSet := countSet(c.GitHubAppID != 0, c.GitHubInstallationID != 0, c.GitHubPrivateKeyPath != "")
	opRefSet := 0
	ppRefSet := 0
	if c.OnePassword != nil {
		opRefSet = countSet(
			c.OnePassword.GitHubAppIDRef != "",
			c.OnePassword.GitHubInstallationRef != "",
			c.OnePassword.GitHubPrivateKeyRef != "",
		)
	}
	if c.ProtonPass != nil {
		ppRefSet = countSet(
			c.ProtonPass.GitHubAppIDRef != "",
			c.ProtonPass.GitHubInstallationRef != "",
			c.ProtonPass.GitHubPrivateKeyRef != "",
		)
	}

	if err := validateGitHubAppMode(directSet, opRefSet, ppRefSet); err != nil {
		return err
	}
	if directSet == 0 && opRefSet == 0 && ppRefSet == 0 {
		return nil
	}
	if c.GitTokenPath != "" {
		return errors.New("github_app_id and git_token_path are mutually exclusive")
	}
	if c.OnePassword != nil && c.OnePassword.GitTokenRef != "" {
		return errors.New("github_app_id and onepassword.git_token_ref are mutually exclusive")
	}
	if c.ProtonPass != nil && c.ProtonPass.GitTokenRef != "" {
		return errors.New("github_app_id and protonpass.git_token_ref are mutually exclusive")
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

// HasGitHubAppPPRefs reports whether GitHub App credentials are configured via Proton Pass refs.
func (c *Config) HasGitHubAppPPRefs() bool {
	if c.ProtonPass == nil {
		return false
	}
	return c.ProtonPass.GitHubAppIDRef != "" && c.ProtonPass.GitHubInstallationRef != "" && c.ProtonPass.GitHubPrivateKeyRef != ""
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

func validateOptionalPassRef(name, ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := pp.ParseRef(ref); err != nil {
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

func validateGitHubAppMode(directSet, opRefSet, ppRefSet int) error {
	configured := 0
	if directSet > 0 {
		configured++
	}
	if opRefSet > 0 {
		configured++
	}
	if ppRefSet > 0 {
		configured++
	}
	if configured > 1 {
		return errors.New("github app must be configured via exactly one of: direct fields, onepassword refs, or protonpass refs (each pair is mutually exclusive)")
	}
	switch {
	case directSet > 0 && directSet < 3:
		return errors.New("all GitHub App fields must be set together (github_app_id, github_installation_id, github_private_key_path)")
	case opRefSet > 0 && opRefSet < 3:
		return errors.New("all onepassword github app refs must be set together (onepassword.github_app_id_ref, onepassword.github_installation_id_ref, onepassword.github_private_key_ref)")
	case ppRefSet > 0 && ppRefSet < 3:
		return errors.New("all protonpass github app refs must be set together (protonpass.github_app_id_ref, protonpass.github_installation_id_ref, protonpass.github_private_key_ref)")
	}
	return nil
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
