package agentcfg

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // table-driven test
func TestLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    Config
		wantErr string
	}{
		{
			name: "full config",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
repo_branch: develop
git_token_path: /etc/picolet/git-token
poll_interval: 30s
metrics_port: 9418
secrets_dir: /run/secrets
webhook_secret_path: /etc/picolet/webhook-secret
`,
			//nolint:gosec // G101: test fixture, not real credentials
			want: Config{
				Hostname:          "rpi5-1",
				RepoURL:           "https://github.com/example/fleet.git",
				RepoBranch:        "develop",
				GitTokenPath:      "/etc/picolet/git-token",
				PollInterval:      30 * time.Second,
				MetricsPort:       9418,
				SecretsDir:        "/run/secrets",
				PodmanSocket:      "/run/podman/podman.sock",
				WebhookSecretPath: "/etc/picolet/webhook-secret",
				SystemdUser:       new(false),
			},
		},
		{
			name: "defaults applied",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
			},
		},
		{
			name: "systemd_user explicit true overrides rootless false",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
rootless: false
systemd_user: true
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(true),
			},
		},
		{
			name: "systemd_user explicit false overrides rootless true",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
rootless: true
systemd_user: false
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				Rootless:     true,
				SystemdUser:  new(false),
			},
		},
		{
			name: "systemd_user defaults to rootless when unset",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
rootless: true
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				Rootless:     true,
				SystemdUser:  new(true),
			},
		},
		{
			name: "repo_sub_dir and data_dir parsed",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
repo_sub_dir: fleet/config
data_dir: /tmp/picolet-data
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				RepoSubDir:   "fleet/config",
				DataDir:      "/tmp/picolet-data",
				SystemdUser:  new(false),
			},
		},
		{
			name:    "missing hostname",
			content: "repo_url: https://example.com/repo.git\n",
			wantErr: "hostname is required",
		},
		{
			name: "repo_sub_dir rejects absolute path",
			content: `
hostname: rpi5-1
repo_sub_dir: /etc/passwd
`,
			wantErr: "repo_sub_dir must be a relative path within the repo",
		},
		{
			name: "repo_sub_dir rejects parent traversal",
			content: `
hostname: rpi5-1
repo_sub_dir: "../../../etc"
`,
			wantErr: "repo_sub_dir must be a relative path within the repo",
		},
		{
			name: "github app config valid",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			want: Config{
				Hostname:             "rpi5-1",
				RepoURL:              "https://github.com/example/fleet.git",
				RepoBranch:           "main",
				PollInterval:         5 * time.Minute,
				MetricsPort:          9417,
				SecretsDir:           "/etc/picolet/secrets",
				PodmanSocket:         "/run/podman/podman.sock",
				GitHubAppID:          12345,
				GitHubInstallationID: 67890,
				GitHubPrivateKeyPath: "/etc/picolet/secrets/github-app.pem",
				SystemdUser:          new(false),
			},
		},
		{
			name: "github app partial config rejected",
			content: `
hostname: rpi5-1
github_app_id: 12345
`,
			wantErr: "all GitHub App fields must be set together",
		},
		{
			name: "github app and git_token_path mutually exclusive",
			content: `
hostname: rpi5-1
git_token_path: /etc/picolet/git-token
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "github app id must be positive",
			content: `
hostname: rpi5-1
github_app_id: -1
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			wantErr: "github_app_id must be positive",
		},
		{
			name: "onepassword config",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
				OnePassword:  &OnePasswordConfig{TokenPath: "/etc/picolet/op-token", RefreshInterval: 6 * time.Hour}, //nolint:gosec // test fixture
			},
		},
		{
			name: "onepassword missing token_path",
			content: `
hostname: rpi5-1
onepassword: {}
`,
			wantErr: "onepassword.token_path is required",
		},
		{
			name: "onepassword negative refresh_interval",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: -5m
`,
			wantErr: "onepassword.refresh_interval must be at least 1m",
		},
		{
			name: "onepassword sub-minute refresh_interval",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: 30s
`,
			wantErr: "onepassword.refresh_interval must be at least 1m",
		},
		{
			name: "onepassword valid refresh_interval",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: 1h
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
				OnePassword:  &OnePasswordConfig{TokenPath: "/etc/picolet/op-token", RefreshInterval: time.Hour}, //nolint:gosec // test fixture
			},
		},
		{
			name:    "minimal config without repo_url",
			content: "hostname: rpi5-1\n",
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
			},
		},
		{
			name: "onepassword git_token_ref valid",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/token"
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture
					TokenPath:       "/etc/picolet/op-token",
					RefreshInterval: 6 * time.Hour,
					GitTokenRef:     "op://vault/item/token",
				},
			},
		},
		{
			name: "onepassword git_token_ref invalid ref missing field",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item"
`,
			wantErr: "onepassword.git_token_ref",
		},
		{
			name: "onepassword git_token_ref and git_token_path mutually exclusive",
			content: `
hostname: rpi5-1
git_token_path: /etc/picolet/git-token
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/token"
`,
			wantErr: "git_token_path and onepassword.git_token_ref are mutually exclusive",
		},
		{
			name: "onepassword git_token_ref without git_token_path succeeds",
			content: `
hostname: rpi5-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/credential"
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:     "rpi5-1",
				RepoBranch:   "main",
				PollInterval: 5 * time.Minute,
				MetricsPort:  9417,
				SecretsDir:   "/etc/picolet/secrets",
				PodmanSocket: "/run/podman/podman.sock",
				SystemdUser:  new(false),
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture
					TokenPath:       "/etc/picolet/op-token",
					RefreshInterval: 6 * time.Hour,
					GitTokenRef:     "op://vault/item/credential",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			got, err := Load(path)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, *got)
		})
	}
}

func TestLoadFileNotFound(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/config.yml")
	require.Error(t, err)
}
