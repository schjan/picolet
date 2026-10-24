package resolver

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
)

func TestDirSecretReaderConfinedToDir(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host.key"), []byte("HOST-KEY"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api.token"), []byte("token"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "host.key"), filepath.Join(dir, "link.key")))

	read, release := DirSecretReader(dir, nil)
	defer release()

	for range 2 { // a reader serves every secret of a resolve pass
		got, err := read("api.token")
		require.NoError(t, err)
		assert.Equal(t, "token", got)
	}
	for _, name := range []string{"../" + filepath.Base(outside) + "/host.key", "link.key"} {
		got, err := read(name)
		require.Error(t, err, name)
		assert.Empty(t, got, name)
	}
}

func TestDirSecretReaderMissingDirOnlyFailsOnRead(t *testing.T) {
	t.Parallel()
	read, release := DirSecretReader(filepath.Join(t.TempDir(), "absent"), nil)
	defer release() // nothing opened: must not panic

	_, err := read("api.token")
	require.ErrorContains(t, err, "opening secrets dir")
}

const refusedMsg = "Agent credentials cannot be deployed"

// credentialKinds lists every Agent credential setting; set points it at a
// path. A dir credential (the session dir) is a directory, so the Fleet
// reads a file below it.
var credentialKinds = []struct {
	setting string
	dir     bool
	set     func(c *agentcfg.Config, path string)
}{
	{"onepassword.token_path", false, func(c *agentcfg.Config, p string) {
		c.OnePassword = &agentcfg.OnePasswordConfig{TokenPath: p}
	}},
	{"protonpass.pat_path", false, func(c *agentcfg.Config, p string) {
		c.ProtonPass = &agentcfg.ProtonPassConfig{PATPath: p}
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
}

// secretsWithCredential lays out a secrets dir holding the credential at
// "cred" (a directory with a "local.key" for a dir credential) and an
// ordinary secret "api.token", and an Agent config naming the credential.
func secretsWithCredential(t *testing.T, setting string) (dir, credFile string, cfg *agentcfg.Config) {
	t.Helper()
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api.token"), []byte("token"), 0o600))
	cfg = &agentcfg.Config{}
	for _, k := range credentialKinds {
		if k.setting != setting {
			continue
		}
		credFile = "cred"
		if k.dir {
			require.NoError(t, os.Mkdir(filepath.Join(dir, "cred"), 0o700))
			credFile = "cred/local.key" //nolint:gosec // G101: a test file name, not a credential
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, credFile), []byte("CREDENTIAL"), 0o600))
		k.set(cfg, filepath.Join(dir, "cred"))
		return dir, credFile, cfg
	}
	t.Fatalf("unknown credential setting %q", setting)
	return "", "", nil
}

func assertRefused(t *testing.T, err error, name, setting string) {
	t.Helper()
	require.ErrorContains(t, err, `"`+name+`"`)
	require.ErrorContains(t, err, setting)
	require.ErrorContains(t, err, refusedMsg)
}

func readSecretFileFleet(t *testing.T, secret string) (fstest.MapFS, *config.Config) {
	t.Helper()
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports:\n  picolet_system_metrics: 9418\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  paths:
    - files/app.env.tmpl
roles: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte("hostname: test-host\nrole: server\nfeatures: []\n")},
		"files/app.env.tmpl":       &fstest.MapFile{Data: []byte(`TOKEN={{ readSecretFile "` + secret + `" }}` + "\n")},
	}
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	return fsys, cfg
}

func TestResolveHostRefusesAgentCredentialInReadSecretFile(t *testing.T) {
	t.Parallel()
	for _, k := range credentialKinds {
		t.Run(k.setting, func(t *testing.T) {
			t.Parallel()
			dir, credFile, agentCfg := secretsWithCredential(t, k.setting)
			fsys, cfg := readSecretFileFleet(t, credFile)
			read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
			defer release()
			r, err := New(Config{FS: fsys, Config: cfg, SecretReader: read})
			require.NoError(t, err)

			_, err = r.ResolveHost(t.Context(), "test-host")
			assertRefused(t, err, credFile, k.setting)
		})
	}
}

func TestResolveHostRefusesAgentCredentialAsSecretEntry(t *testing.T) {
	t.Parallel()
	for _, k := range credentialKinds {
		t.Run(k.setting, func(t *testing.T) {
			t.Parallel()
			dir, credFile, agentCfg := secretsWithCredential(t, k.setting)
			// newSecretTestFS's host-only secret entry is host_only.txt.
			require.NoError(t, os.Symlink(credFile, filepath.Join(dir, "host_only.txt")))
			fsys, cfg := newSecretTestFS(t)
			read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
			defer release()
			r, err := New(Config{FS: fsys, Config: cfg, SecretReader: read})
			require.NoError(t, err)

			_, err = r.ResolveHost(t.Context(), "test-host")
			assertRefused(t, err, "host_only.txt", k.setting)
		})
	}
}

func TestResolveHostReadsOtherSecretsBesideAgentCredentials(t *testing.T) {
	t.Parallel()
	dir, _, agentCfg := secretsWithCredential(t, "git_token_path")
	fsys, cfg := readSecretFileFleet(t, "api.token")
	read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
	defer release()
	r, err := New(Config{FS: fsys, Config: cfg, SecretReader: read})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, "TOKEN=token\n", resolved.Files[0].Content)
}

func TestDirSecretReaderRefusesLinksToAgentCredentials(t *testing.T) {
	t.Parallel()
	dir, _, agentCfg := secretsWithCredential(t, "git_token_path")
	require.NoError(t, os.Symlink("cred", filepath.Join(dir, "link")))
	require.NoError(t, os.Link(filepath.Join(dir, "cred"), filepath.Join(dir, "hard")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.Symlink("../cred", filepath.Join(dir, "sub", "up")))

	read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
	defer release()

	for _, name := range []string{"cred", "./cred", "sub/../cred", "link", "hard", "sub/up"} {
		got, err := read(name)
		assertRefused(t, err, name, "git_token_path")
		assert.Empty(t, got, name)
	}
}

func TestDirSecretReaderRefusesEverythingUnderSessionDir(t *testing.T) {
	t.Parallel()
	dir, _, agentCfg := secretsWithCredential(t, "protonpass.session_dir")
	session := filepath.Join(dir, "cred")
	require.NoError(t, os.MkdirAll(filepath.Join(session, "deep"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(session, "deep", "state"), []byte("S"), 0o600))
	require.NoError(t, os.Symlink("cred", filepath.Join(dir, "sess")))
	require.NoError(t, os.Link(filepath.Join(session, "deep", "state"), filepath.Join(dir, "hard")))

	read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
	defer release()

	for _, name := range []string{"cred/local.key", "cred/deep/state", "sess/local.key", "sess/deep/state", "hard"} {
		_, err := read(name)
		assertRefused(t, err, name, "protonpass.session_dir")
	}
	got, err := read("api.token")
	require.NoError(t, err)
	assert.Equal(t, "token", got)
}

func TestDirSecretReaderRefusesSessionDirOutsideSecretsDir(t *testing.T) {
	t.Parallel()
	session := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(session, "local.key"), []byte("K"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.Link(filepath.Join(session, "local.key"), filepath.Join(dir, "hard")))
	agentCfg := &agentcfg.Config{ProtonPass: &agentcfg.ProtonPassConfig{SessionDir: session}}

	read, release := DirSecretReader(dir, agentCfg.RefuseCredential)
	defer release()

	_, err := read("hard")
	assertRefused(t, err, "hard", "protonpass.session_dir")
}
