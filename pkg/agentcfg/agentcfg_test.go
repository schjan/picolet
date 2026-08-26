package agentcfg

import (
	"net"
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
hostname: node-1
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
				Hostname:          "node-1",
				RepoURL:           "https://github.com/example/fleet.git",
				RepoBranch:        "develop",
				GitTokenPath:      "/etc/picolet/git-token",
				PollInterval:      30 * time.Second,
				MetricsPort:       9418,
				SecretsDir:        "/run/secrets",
				PodmanSocket:      "/run/podman/podman.sock",
				WebhookSecretPath: "/etc/picolet/webhook-secret",
				PruneInterval:     7 * 24 * time.Hour,
			},
		},
		{
			name: "defaults applied",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoURL:       "https://github.com/example/fleet.git",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
			},
		},
		{
			name: "systemd_user explicit true overrides rootless false",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
rootless: false
systemd_user: true
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoURL:       "https://github.com/example/fleet.git",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				SystemdUser:   new(true),
			},
		},
		{
			name: "systemd_user explicit false overrides rootless true",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
rootless: true
systemd_user: false
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoURL:       "https://github.com/example/fleet.git",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				Rootless:      true,
				SystemdUser:   new(false),
			},
		},
		{
			name: "systemd_user stays unset for lazy detection",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
rootless: true
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoURL:       "https://github.com/example/fleet.git",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				Rootless:      true,
			},
		},
		{
			name: "repo_sub_dir, data_dir and host_data_dir parsed",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
repo_sub_dir: fleet/config
data_dir: /tmp/picolet-data
host_data_dir: /home/pi/.local/share/picolet
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoURL:       "https://github.com/example/fleet.git",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				RepoSubDir:    "fleet/config",
				DataDir:       "/tmp/picolet-data",
				HostDataDir:   "/home/pi/.local/share/picolet",
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
hostname: node-1
repo_sub_dir: /etc/passwd
`,
			wantErr: "repo_sub_dir must be a relative path within the repo",
		},
		{
			name: "repo_sub_dir rejects parent traversal",
			content: `
hostname: node-1
repo_sub_dir: "../../../etc"
`,
			wantErr: "repo_sub_dir must be a relative path within the repo",
		},
		{
			name: "github app config valid",
			content: `
hostname: node-1
repo_url: https://github.com/example/fleet.git
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:             "node-1",
				RepoURL:              "https://github.com/example/fleet.git",
				RepoBranch:           "main",
				PollInterval:         5 * time.Minute,
				SecretsDir:           "/etc/picolet/secrets",
				PodmanSocket:         "/run/podman/podman.sock",
				GitHubAppID:          12345,
				GitHubInstallationID: 67890,
				GitHubPrivateKeyPath: "/etc/picolet/secrets/github-app.pem",
				PruneInterval:        7 * 24 * time.Hour,
			},
		},
		{
			name: "github app partial config rejected",
			content: `
hostname: node-1
github_app_id: 12345
`,
			wantErr: "all GitHub App fields must be set together",
		},
		{
			name: "github app and git_token_path mutually exclusive",
			content: `
hostname: node-1
git_token_path: /etc/picolet/git-token
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "github app and onepassword git_token_ref mutually exclusive",
			content: `
hostname: node-1
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/token"
`,
			wantErr: "GitHub App and onepassword.git_token_ref are mutually exclusive",
		},
		{
			name: "github app and protonpass git_token_ref mutually exclusive",
			content: `
hostname: node-1
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
protonpass:
  pat_path: /etc/picolet/pp-pat
  git_token_ref: "pass://share/item/token"
`,
			wantErr: "GitHub App and protonpass.git_token_ref are mutually exclusive",
		},
		{
			name: "github app id must be positive",
			content: `
hostname: node-1
github_app_id: -1
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
`,
			wantErr: "github_app_id must be positive",
		},
		{
			name: "github app refs via onepassword valid",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  github_app_id_ref: "op://vault/app/id"
  github_installation_id_ref: "op://vault/app/installation_id"
  github_private_key_ref: "op://vault/app/private_key"
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture, not real credentials
					TokenPath:             "/etc/picolet/op-token",
					RefreshInterval:       6 * time.Hour,
					GitHubAppIDRef:        "op://vault/app/id",
					GitHubInstallationRef: "op://vault/app/installation_id",
					GitHubPrivateKeyRef:   "op://vault/app/private_key",
				},
			},
		},
		{
			name: "github app refs partial rejected",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  github_app_id_ref: "op://vault/app/id"
`,
			wantErr: "all onepassword github app refs must be set together",
		},
		{
			name: "github app refs and direct fields mutually exclusive",
			content: `
hostname: node-1
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/secrets/github-app.pem
onepassword:
  token_path: /etc/picolet/op-token
  github_app_id_ref: "op://vault/app/id"
  github_installation_id_ref: "op://vault/app/installation_id"
  github_private_key_ref: "op://vault/app/private_key"
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "onepassword config",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword:   &OnePasswordConfig{TokenPath: "/etc/picolet/op-token", RefreshInterval: 6 * time.Hour}, //nolint:gosec // test fixture
			},
		},
		{
			name: "onepassword missing token_path",
			content: `
hostname: node-1
onepassword: {}
`,
			wantErr: "onepassword.token_path is required",
		},
		{
			name: "onepassword negative refresh_interval",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: -5m
`,
			wantErr: "onepassword.refresh_interval must be at least 1m",
		},
		{
			name: "onepassword sub-minute refresh_interval",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: 30s
`,
			wantErr: "onepassword.refresh_interval must be at least 1m",
		},
		{
			name: "onepassword valid refresh_interval",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  refresh_interval: 1h
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword:   &OnePasswordConfig{TokenPath: "/etc/picolet/op-token", RefreshInterval: time.Hour}, //nolint:gosec // test fixture
			},
		},
		{
			name:    "minimal config without repo_url",
			content: "hostname: node-1\n",
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
			},
		},
		{
			name: "onepassword git_token_ref valid",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/token"
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
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
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item"
`,
			wantErr: "onepassword.git_token_ref",
		},
		{
			name: "onepassword git_token_ref and git_token_path mutually exclusive",
			content: `
hostname: node-1
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
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/credential"
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture
					TokenPath:       "/etc/picolet/op-token",
					RefreshInterval: 6 * time.Hour,
					GitTokenRef:     "op://vault/item/credential",
				},
			},
		},
		{
			name: "protonpass lazy mode (no PAT) valid",
			content: `
hostname: node-1
protonpass: {}
`,
			want: Config{ //nolint:gosec // test fixture, not real credentials
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				ProtonPass: &ProtonPassConfig{
					RefreshInterval: 6 * time.Hour,
				},
			},
		},
		{
			name: "protonpass git_token_ref invalid format rejected",
			content: `
hostname: node-1
protonpass:
  git_token_ref: "not-a-pass-ref"
`,
			wantErr: "missing pass:// prefix",
		},
		{
			name: "git_token_path and protonpass git_token_ref mutually exclusive",
			content: `
hostname: node-1
git_token_path: /etc/picolet/git-token
protonpass:
  git_token_ref: "pass://share/item/token"
`,
			wantErr: "git_token_path and protonpass.git_token_ref are mutually exclusive",
		},
		{
			name: "onepassword and protonpass git_token_ref mutually exclusive",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  git_token_ref: "op://vault/item/token"
protonpass:
  git_token_ref: "pass://share/item/token"
`,
			wantErr: "onepassword.git_token_ref and protonpass.git_token_ref are mutually exclusive",
		},
		{
			name: "protonpass github app refs partial rejected",
			content: `
hostname: node-1
protonpass:
  github_app_id_ref: "pass://share/item/id"
`,
			wantErr: "all protonpass github app refs must be set together",
		},
		{
			name: "protonpass github app refs and direct fields mutually exclusive",
			content: `
hostname: node-1
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: /etc/picolet/key.pem
protonpass:
  github_app_id_ref: "pass://share/item/id"
  github_installation_id_ref: "pass://share/item/inst"
  github_private_key_ref: "pass://share/item/key"
`,
			wantErr: "github app must be configured via exactly one",
		},
		{
			name: "protonpass and onepassword github app refs mutually exclusive",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  github_app_id_ref: "op://vault/item/id"
  github_installation_id_ref: "op://vault/item/inst"
  github_private_key_ref: "op://vault/item/key"
protonpass:
  github_app_id_ref: "pass://share/item/id"
  github_installation_id_ref: "pass://share/item/inst"
  github_private_key_ref: "pass://share/item/key"
`,
			wantErr: "github app must be configured via exactly one",
		},
		{
			name: "protonpass github app refs valid",
			content: `
hostname: node-1
protonpass:
  pat_path: /etc/picolet/pp-pat
  github_app_id_ref: "pass://share/item/id"
  github_installation_id_ref: "pass://share/item/inst"
  github_private_key_ref: "pass://share/item/key"
`,
			want: Config{ //nolint:gosec // test fixture
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				ProtonPass: &ProtonPassConfig{
					PATPath:               "/etc/picolet/pp-pat",
					RefreshInterval:       6 * time.Hour,
					GitHubAppIDRef:        "pass://share/item/id",
					GitHubInstallationRef: "pass://share/item/inst",
					GitHubPrivateKeyRef:   "pass://share/item/key",
				},
			},
		},
		{
			name: "onepassword and protonpass coexist when no resource conflicts",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
protonpass:
  pat_path: /etc/picolet/pp-pat
`,
			want: Config{ //nolint:gosec // test fixture
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture
					TokenPath:       "/etc/picolet/op-token",
					RefreshInterval: 6 * time.Hour,
				},
				ProtonPass: &ProtonPassConfig{
					PATPath:         "/etc/picolet/pp-pat",
					RefreshInterval: 6 * time.Hour,
				},
			},
		},
		{
			name: "credential expiry timestamps parsed",
			content: `
hostname: node-1
onepassword:
  token_path: /etc/picolet/op-token
  token_expires_at: 2026-12-31T23:59:59Z
protonpass:
  pat_path: /etc/picolet/pp-pat
  pat_expires_at: 2026-09-15T00:00:00Z
`,
			want: Config{ //nolint:gosec // test fixture
				Hostname:      "node-1",
				RepoBranch:    "main",
				PollInterval:  5 * time.Minute,
				SecretsDir:    "/etc/picolet/secrets",
				PodmanSocket:  "/run/podman/podman.sock",
				PruneInterval: 7 * 24 * time.Hour,
				OnePassword: &OnePasswordConfig{ //nolint:gosec // test fixture
					TokenPath:       "/etc/picolet/op-token",
					TokenExpiresAt:  time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
					RefreshInterval: 6 * time.Hour,
				},
				ProtonPass: &ProtonPassConfig{
					PATPath:         "/etc/picolet/pp-pat",
					PATExpiresAt:    time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
					RefreshInterval: 6 * time.Hour,
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

func TestPruneConfig(t *testing.T) {
	t.Parallel()

	t.Run("defaults to enabled weekly", func(t *testing.T) {
		t.Parallel()
		cfg, err := Parse([]byte("hostname: node-1\n"))
		require.NoError(t, err)
		assert.True(t, cfg.PruneImagesEnabled())
		assert.Equal(t, 7*24*time.Hour, cfg.PruneInterval)
	})

	t.Run("explicit disable", func(t *testing.T) {
		t.Parallel()
		cfg, err := Parse([]byte("hostname: node-1\nprune_images: false\n"))
		require.NoError(t, err)
		assert.False(t, cfg.PruneImagesEnabled())
	})

	t.Run("custom interval", func(t *testing.T) {
		t.Parallel()
		cfg, err := Parse([]byte("hostname: node-1\nprune_interval: 24h\n"))
		require.NoError(t, err)
		assert.Equal(t, 24*time.Hour, cfg.PruneInterval)
	})

	t.Run("rejects sub-minute interval", func(t *testing.T) {
		t.Parallel()
		_, err := Parse([]byte("hostname: node-1\nprune_interval: 30s\n"))
		require.Error(t, err)
		assert.ErrorContains(t, err, "prune_interval must be at least 1m")
	})

	t.Run("sub-minute interval allowed when disabled", func(t *testing.T) {
		t.Parallel()
		_, err := Parse([]byte("hostname: node-1\nprune_images: false\nprune_interval: 30s\n"))
		require.NoError(t, err)
	})
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
hostname: node-1
repo_url: https://github.com/example/fleet.git
not_a_field: true
`), 0o600))
	_, err := Load(path)
	require.Error(t, err)
	require.ErrorContains(t, err, "not_a_field")
}

func TestHasGitHubAppOPRefsDetectsConfigured(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		OnePassword: &OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	assert.True(t, cfg.HasGitHubApp())
	assert.True(t, cfg.HasGitHubAppOPRefs())
	assert.False(t, cfg.HasGitHubAppPPRefs())
}

func TestHasGitHubAppIncludesProtonPassRefs(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ProtonPass: &ProtonPassConfig{
			GitHubAppIDRef:        "pass://share/app/id",
			GitHubInstallationRef: "pass://share/app/installation",
			GitHubPrivateKeyRef:   "pass://share/app/private_key",
		},
	}

	assert.True(t, cfg.HasGitHubApp())
	assert.True(t, cfg.HasGitHubAppPPRefs())
}

func TestLoadFileNotFound(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/config.yml")
	require.Error(t, err)
}

func TestValidateAppliesDefaultsForZeroRefreshInterval(t *testing.T) {
	t.Parallel()
	// Validate must be safe to call directly on a programmatically-constructed
	// Config without first invoking Load (which would have run setDefaults).
	cfg := &Config{
		Hostname:    "node-1",
		OnePassword: &OnePasswordConfig{TokenPath: "/tmp/op"},
		ProtonPass:  &ProtonPassConfig{}, // RefreshInterval zero
	}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 6*time.Hour, cfg.OnePassword.RefreshInterval)
	assert.Equal(t, 6*time.Hour, cfg.ProtonPass.RefreshInterval)
}

// Mutates the package-level detection hooks, so it must not run in parallel
// with other tests.
//
//nolint:paralleltest // see above
func TestLazyDetectionAccessors(t *testing.T) {
	oldDetectSystemdUser := detectSystemdUserFunc
	oldDetectHostDataDir := detectHostDataDirFunc
	t.Cleanup(func() {
		detectSystemdUserFunc = oldDetectSystemdUser
		detectHostDataDirFunc = oldDetectHostDataDir
	})
	detectSystemdUserFunc = func() bool { return true }
	detectHostDataDirFunc = func(dataDir string) string {
		assert.Equal(t, "/var/lib/picolet", dataDir)
		return "/home/pi/.local/share/picolet"
	}

	cfg := &Config{}
	assert.True(t, cfg.UseSystemdUser())
	assert.Equal(t, "/home/pi/.local/share/picolet", cfg.EffectiveHostDataDir())

	// Explicit config wins over detection.
	cfg = &Config{SystemdUser: new(false), HostDataDir: "/srv/picolet"}
	assert.False(t, cfg.UseSystemdUser())
	assert.Equal(t, "/srv/picolet", cfg.EffectiveHostDataDir())
}

//nolint:funlen // table-driven test
func TestListenAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		content  string
		wantAddr string
		wantDial string
		wantErr  string
	}{
		{
			name:     "unset binds loopback on the default port",
			content:  "hostname: node-1\n",
			wantAddr: "127.0.0.1:9417",
			wantDial: "127.0.0.1:9417",
		},
		{
			name:     "metrics_port alone keeps loopback",
			content:  "hostname: node-1\nmetrics_port: 9418\n",
			wantAddr: "127.0.0.1:9418",
			wantDial: "127.0.0.1:9418",
		},
		{
			name:     "listen_addr with port wins and probes loopback",
			content:  "hostname: node-1\nlisten_addr: 0.0.0.0:9500\n",
			wantAddr: "0.0.0.0:9500",
			wantDial: "127.0.0.1:9500",
		},
		{
			name:     "portless listen_addr takes the port from metrics_port",
			content:  "hostname: node-1\nlisten_addr: 0.0.0.0\nmetrics_port: 9418\n",
			wantAddr: "0.0.0.0:9418",
			wantDial: "127.0.0.1:9418",
		},
		{
			name:     "portless listen_addr falls back to the default port",
			content:  "hostname: node-1\nlisten_addr: 192.168.1.10\n",
			wantAddr: "192.168.1.10:9417",
			wantDial: "192.168.1.10:9417",
		},
		{
			name:     "agreeing listen_addr and metrics_port",
			content:  "hostname: node-1\nlisten_addr: 127.0.0.1:9418\nmetrics_port: 9418\n",
			wantAddr: "127.0.0.1:9418",
			wantDial: "127.0.0.1:9418",
		},
		{
			name:    "disagreeing listen_addr and metrics_port",
			content: "hostname: node-1\nlisten_addr: 0.0.0.0:9500\nmetrics_port: 9418\n",
			wantErr: "listen_addr port 9500 disagrees with metrics_port 9418",
		},
		{
			name:     "empty host means all interfaces",
			content:  "hostname: node-1\nlisten_addr: \":9417\"\n",
			wantAddr: ":9417",
			wantDial: "127.0.0.1:9417",
		},
		{
			name:     "ipv6 wildcard is probed on loopback",
			content:  "hostname: node-1\nlisten_addr: \"[::]:9417\"\n",
			wantAddr: "[::]:9417",
			wantDial: "127.0.0.1:9417",
		},
		{
			name:     "metrics_port zero keeps meaning the default",
			content:  "hostname: node-1\nmetrics_port: 0\n",
			wantAddr: "127.0.0.1:9417",
			wantDial: "127.0.0.1:9417",
		},
		{
			name:    "zoned link-local address is rejected",
			content: "hostname: node-1\nlisten_addr: \"[fe80::1%en0]:9417\"\n",
			wantErr: "listen_addr must be host:port or a host",
		},
		{
			name:    "listen_addr port zero is rejected",
			content: "hostname: node-1\nlisten_addr: 127.0.0.1:0\n",
			wantErr: "listen_addr port must not be 0",
		},
		{
			name:    "listen_addr port out of range",
			content: "hostname: node-1\nlisten_addr: 127.0.0.1:99999\n",
			wantErr: "listen_addr port must be a decimal port between 1 and 65535",
		},
		{
			name:    "signed listen_addr port is rejected",
			content: "hostname: node-1\nlisten_addr: \"127.0.0.1:+9417\"\n",
			wantErr: "listen_addr port must be a decimal port between 1 and 65535",
		},
		{
			name:    "hex listen_addr port is rejected",
			content: "hostname: node-1\nlisten_addr: \"127.0.0.1:0x10\"\n",
			wantErr: "listen_addr port must be a decimal port between 1 and 65535",
		},
		{
			name:    "metrics_port out of range",
			content: "hostname: node-1\nmetrics_port: 70000\n",
			wantErr: "metrics_port must be between 0 and 65535",
		},
		{
			name:    "unparseable listen_addr",
			content: "hostname: node-1\nlisten_addr: \"not:an:addr\"\n",
			wantErr: "listen_addr must be host:port or a host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(tt.content))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantAddr, cfg.EffectiveListenAddr())
			assert.Equal(t, tt.wantDial, cfg.DialAddr())
		})
	}
}

func TestPrivateNetworkNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		addrs []net.IP
		want  bool
	}{
		{name: "no routable address is inconclusive", addrs: nil, want: false},
		{name: "rootful podman bridge address", addrs: []net.IP{net.ParseIP("10.88.0.7")}, want: true},
		{name: "rootless pasta address", addrs: []net.IP{net.ParseIP("10.0.2.100")}, want: true},
		{name: "machine LAN address", addrs: []net.IP{net.ParseIP("192.168.1.20")}, want: false},
		{
			name:  "host namespace with podman bridge alongside a NIC",
			addrs: []net.IP{net.ParseIP("10.88.0.1"), net.ParseIP("192.168.1.20")},
			want:  false,
		},
		{name: "global ipv6 address", addrs: []net.IP{net.ParseIP("2001:db8::5")}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, privateNetworkNamespace(tt.addrs))
		})
	}
}
