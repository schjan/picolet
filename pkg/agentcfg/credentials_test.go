package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/schjan/picolet/pkg/protonpass"
)

func TestCredentialsListsEveryCredentialSetting(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
hostname: node-1
git_token_path: /s/git.token
webhook_secret_path: /s/webhook
mqtt:
  broker_url: tcp://broker:1883
  username: picolet
  password_path: /s/mqtt.pw
onepassword:
  token_path: /s/op.token
protonpass:
  cli_path: /usr/bin/pass-cli
  pat_path: /s/pass.pat
  session_dir: /var/lib/pass/.session
`))
	require.NoError(t, err)
	assert.ElementsMatch(t, []Credential{
		{Setting: "onepassword.token_path", Path: "/s/op.token"},
		{Setting: "protonpass.pat_path", Path: "/s/pass.pat"},
		{Setting: "protonpass.session_dir", Path: "/var/lib/pass/.session", Dir: true},
		{Setting: "git_token_path", Path: "/s/git.token"},
		{Setting: "webhook_secret_path", Path: "/s/webhook"},
		{Setting: "mqtt.password_path", Path: "/s/mqtt.pw"},
	}, cfg.Credentials())
}

func TestCredentialsListsGitHubAppPrivateKey(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
hostname: node-1
github_app_id: 1
github_installation_id: 2
github_private_key_path: /s/app.pem
`))
	require.NoError(t, err)
	assert.Equal(t, []Credential{{Setting: "github_private_key_path", Path: "/s/app.pem"}}, cfg.Credentials())
}

func TestCredentialsUsesDefaultSessionDirInPATMode(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte("hostname: node-1\nprotonpass:\n  pat_path: /s/pass.pat\n"))
	require.NoError(t, err)
	assert.Contains(t, cfg.Credentials(), Credential{Setting: "protonpass.session_dir", Path: pp.DefaultSessionDir, Dir: true})
}

func TestCredentialsEmptyWithoutCredentialSettings(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte("hostname: node-1\nprotonpass:\n  cli_path: /usr/bin/pass-cli\n"))
	require.NoError(t, err)
	assert.Empty(t, cfg.Credentials())
}

// writeConfig writes an Agent config whose credential settings are filled
// in with the given paths (placeholders {a} and {b}).
func writeConfig(t *testing.T, tmpl, a, b string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := strings.NewReplacer("{a}", a, "{b}", b).Replace(tmpl)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

//nolint:funlen // table-driven test
func TestLoadRejectsCredentialSettingsNamingSameFile(t *testing.T) {
	t.Parallel()
	pairs := []struct {
		name               string
		tmpl               string
		settingA, settingB string
		bUnderSessionDir   bool
	}{
		{
			name:     "git token and webhook secret",
			tmpl:     "hostname: n\ngit_token_path: {a}\nwebhook_secret_path: {b}\n",
			settingA: "git_token_path", settingB: "webhook_secret_path",
		},
		{
			name:     "mqtt password and 1Password token",
			tmpl:     "hostname: n\nmqtt:\n  broker_url: tcp://b:1883\n  username: u\n  password_path: {a}\nonepassword:\n  token_path: {b}\n",
			settingA: "mqtt.password_path", settingB: "onepassword.token_path",
		},
		{
			name:     "GitHub App key and Proton Pass PAT",
			tmpl:     "hostname: n\ngithub_app_id: 1\ngithub_installation_id: 2\ngithub_private_key_path: {a}\nprotonpass:\n  pat_path: {b}\n  session_dir: /nonexistent/session\n",
			settingA: "github_private_key_path", settingB: "protonpass.pat_path",
		},
		{
			name:     "git token inside Proton Pass session dir",
			tmpl:     "hostname: n\ngit_token_path: {a}\nprotonpass:\n  session_dir: {b}\n",
			settingA: "git_token_path", settingB: "protonpass.session_dir",
			bUnderSessionDir: true,
		},
	}
	spellings := map[string]func(t *testing.T, dir, target string) string{
		"same path": func(_ *testing.T, _, target string) string { return target },
		"differently spelled": func(_ *testing.T, dir, target string) string {
			return dir + "/x/../" + filepath.Base(target)
		},
		"symlink": func(t *testing.T, dir, target string) string {
			t.Helper()
			link := filepath.Join(dir, "link")
			require.NoError(t, os.Symlink(target, link))
			return link
		},
		"hard link": func(t *testing.T, dir, target string) string {
			t.Helper()
			if fi, err := os.Stat(target); err == nil && fi.IsDir() {
				return target // directories cannot be hard-linked
			}
			link := filepath.Join(dir, "hard")
			require.NoError(t, os.Link(target, link))
			return link
		},
	}
	for _, p := range pairs {
		for spelling, spell := range spellings {
			t.Run(p.name+"/"+spelling, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(dir, "x"), 0o700))
				b := filepath.Join(dir, "cred")
				a := b
				if p.bUnderSessionDir {
					require.NoError(t, os.MkdirAll(filepath.Join(b, "sub"), 0o700))
					a = filepath.Join(b, "sub", "local.key")
				}
				require.NoError(t, os.WriteFile(a, []byte("secret"), 0o600))
				if p.bUnderSessionDir {
					b = spell(t, dir, b)
				} else {
					a = spell(t, dir, a)
				}

				_, err := Load(writeConfig(t, p.tmpl, a, b))
				require.Error(t, err)
				require.ErrorContains(t, err, p.settingA)
				require.ErrorContains(t, err, p.settingB)
			})
		}
	}
}

func TestLoadAcceptsDistinctAndMissingCredentialFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := filepath.Join(dir, "git.token")
	require.NoError(t, os.WriteFile(a, []byte("a"), 0o600))
	b := filepath.Join(dir, "webhook")
	require.NoError(t, os.WriteFile(b, []byte("b"), 0o600))

	_, err := Load(writeConfig(t, "hostname: n\ngit_token_path: {a}\nwebhook_secret_path: {b}\nmqtt:\n  broker_url: tcp://b:1883\n  username: u\n  password_path: /nonexistent/pw\nonepassword:\n  token_path: /nonexistent/pw2\n", a, b))
	require.NoError(t, err)
}

func TestLoadRejectsCredentialSettingsNamingSameMissingPath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := Load(writeConfig(t, "hostname: n\ngit_token_path: {a}\nwebhook_secret_path: {b}\n", missing, missing+"/../absent"))
	require.ErrorContains(t, err, "git_token_path")
	require.ErrorContains(t, err, "webhook_secret_path")
}
