package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/state"
)

// credentialFleet writes a Fleet whose file reads the host secret secret
// with readSecretFile and whose base group deploys the host secret entry.
func credentialFleet(t *testing.T, readSecret, entry string) string {
	t.Helper()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
	assignments := "base:\n  paths:\n    - files/app.env.tmpl\n"
	if entry != "" {
		assignments += "  secrets:\n    - secrets/" + entry + "\n"
	}
	writeTestFile(t, repoDir, "assignments.yml", assignments)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", "hostname: test-host\nrole: server\nfeatures: []\n")
	writeTestFile(t, repoDir, "files/app.env.tmpl", `TOKEN={{ readSecretFile "`+readSecret+`" }}`+"\n")
	return repoDir
}

func TestAgentRefusesToDeployItsOwnCredentials(t *testing.T) {
	t.Parallel()
	kinds := []struct {
		setting string
		dir     bool
		set     func(c *agentcfg.Config, path string)
	}{
		{"onepassword.token_path", false, func(c *agentcfg.Config, p string) {
			c.OnePassword = &agentcfg.OnePasswordConfig{TokenPath: p}
		}},
		{"protonpass.pat_path", false, func(c *agentcfg.Config, p string) {
			c.ProtonPass = &agentcfg.ProtonPassConfig{PATPath: p, SessionDir: filepath.Join(p, "..", "no-session")}
		}},
		{"protonpass.session_dir", true, func(c *agentcfg.Config, p string) {
			c.ProtonPass = &agentcfg.ProtonPassConfig{SessionDir: p}
		}},
		{"git_token_path", false, func(c *agentcfg.Config, p string) { c.GitTokenPath = p }},
		{"github_private_key_path", false, func(c *agentcfg.Config, p string) { c.GitHubPrivateKeyPath = p }},
		{"webhook_secret_path", false, func(c *agentcfg.Config, p string) { c.WebhookSecretPath = p }},
		{"mqtt.password_path", false, func(c *agentcfg.Config, p string) {
			c.MQTT = &agentcfg.MQTTConfig{PasswordPath: p}
		}},
		{"sops.age_identity_path", false, func(c *agentcfg.Config, p string) {
			c.Sops = &agentcfg.SopsConfig{AgeIdentityPath: p}
		}},
	}
	for _, k := range kinds {
		for _, via := range []string{"readSecretFile", "secrets entry"} {
			t.Run(k.setting+"/"+via, func(t *testing.T) {
				t.Parallel()
				secretsDir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "api.token"), []byte("token"), 0o600))
				cred := filepath.Join(secretsDir, "cred")
				credFile := cred
				if k.dir {
					require.NoError(t, os.Mkdir(cred, 0o700))
					credFile = filepath.Join(cred, "local.key")
				}
				require.NoError(t, os.WriteFile(credFile, []byte("CREDENTIAL"), 0o600))
				rel, err := filepath.Rel(secretsDir, credFile)
				require.NoError(t, err)
				require.NoError(t, os.Symlink(rel, filepath.Join(secretsDir, "leak")))

				repoDir := credentialFleet(t, "leak", "")
				if via == "secrets entry" {
					repoDir = credentialFleet(t, "api.token", "leak")
				}
				cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: secretsDir}
				k.set(cfg, cred)
				a := newTestAgent(t, cfg, WithRepoPath(repoDir))

				st := state.NewState()
				_, err = a.ReconcileOnce(t.Context(), "sha", st, state.NewStore(filepath.Join(t.TempDir(), "state.json")))
				require.ErrorContains(t, err, `"leak"`)
				require.ErrorContains(t, err, k.setting)
				require.ErrorContains(t, err, "Agent credentials cannot be deployed")
			})
		}
	}
}

func TestAgentStillResolvesOtherHostSecrets(t *testing.T) {
	t.Parallel()
	secretsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "api.token"), []byte("token"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "git.token"), []byte("GIT"), 0o600))
	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: secretsDir, GitTokenPath: filepath.Join(secretsDir, "git.token")}
	a := newTestAgent(t, cfg, WithRepoPath(credentialFleet(t, "api.token", "")))

	resolved, _, err := a.loadAndResolve(t.Context())
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, "TOKEN=token\n", resolved.Files[0].Content)
}
