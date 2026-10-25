package agentcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckRenderedAcceptsExistingCredentialFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"git.token", "webhook", "mqtt.pw", "op.token", "pass.pat"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("secret\n"), 0o600))
	}
	err := CheckRenderedConfig([]byte(`
hostname: node-1
repo_url: https://example.com/fleet.git
git_token_path: ` + filepath.Join(dir, "git.token") + `
webhook_secret_path: ` + filepath.Join(dir, "webhook") + `
mqtt:
  broker_url: tcp://broker:1883
  username: picolet
  password_path: ` + filepath.Join(dir, "mqtt.pw") + `
onepassword:
  token_path: ` + filepath.Join(dir, "op.token") + `
protonpass:
  pat_path: ` + filepath.Join(dir, "pass.pat") + `
  session_dir: ` + filepath.Join(dir, "no-session-yet") + `
`))
	assert.NoError(t, err, "a session dir pass-cli has not created yet is not a missing credential")
}

func TestCheckRenderedRejectsMissingCredentialFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "git.token")
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\ngit_token_path: " + missing + "\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git_token_path")
	assert.Contains(t, err.Error(), missing)
}

func TestCheckRenderedRejectsUnreadableCredentialFile(t *testing.T) {
	t.Parallel()
	// A directory where the file should be: unreadable even for root, which
	// a chmod 000 file is not.
	unreadable := filepath.Join(t.TempDir(), "mqtt.pw")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nmqtt:\n  broker_url: tcp://b:1883\n  username: u\n  password_path: " + unreadable + "\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mqtt.password_path")
	assert.Contains(t, err.Error(), unreadable)
}

func TestCheckRenderedRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_key_ever")
}

func TestCheckRenderedRejectsOverlappingCredentials(t *testing.T) {
	t.Parallel()
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("t"), 0o600))
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\ngit_token_path: " + token + "\nwebhook_secret_path: " + token + "\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same credential file")
}

func TestCheckRenderedRejectsMissingRepoURL(t *testing.T) {
	t.Parallel()
	err := CheckRenderedConfig([]byte("hostname: node-1\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo_url is required")
}
