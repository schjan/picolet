package agentcfg

import (
	"errors"
	"io/fs"
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
git_token_path: `+filepath.Join(dir, "git.token")+`
webhook_secret_path: `+filepath.Join(dir, "webhook")+`
mqtt:
  broker_url: tcp://broker:1883
  username: picolet
  password_path: `+filepath.Join(dir, "mqtt.pw")+`
onepassword:
  token_path: `+filepath.Join(dir, "op.token")+`
protonpass:
  pat_path: `+filepath.Join(dir, "pass.pat")+`
  session_dir: `+filepath.Join(dir, "no-session-yet")+`
`), nil)
	assert.NoError(t, err, "a session dir pass-cli has not created yet is not a missing credential")
}

func TestCheckRenderedRejectsMissingCredentialFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "git.token")
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\ngit_token_path: "+missing+"\n"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git_token_path")
	assert.Contains(t, err.Error(), missing)
}

// Every failing check is reported, each its own entry in Failures.
func TestCheckRenderedReportsEveryFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := filepath.Join(dir, "git.token")
	webhook := filepath.Join(dir, "webhook")
	err := CheckRenderedConfig([]byte("hostname: node-1\ngit_token_path: "+git+"\nwebhook_secret_path: "+webhook+"\n"), nil)
	var failures Failures
	require.ErrorAs(t, err, &failures)
	require.Len(t, failures, 3, err.Error())
	assert.Contains(t, failures[0].Error(), "repo_url is required")
	assert.Contains(t, failures[1].Error(), "git_token_path ("+git+")")
	assert.Contains(t, failures[2].Error(), "webhook_secret_path ("+webhook+")")
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.NotContains(t, err.Error(), "\n", "one line, for dashboard events and MQTT")
}

func TestCheckRenderedRejectsUnreadableCredentialFile(t *testing.T) {
	t.Parallel()
	// A directory where the file should be: unreadable even for root, which
	// a chmod 000 file is not.
	unreadable := filepath.Join(t.TempDir(), "mqtt.pw")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nmqtt:\n  broker_url: tcp://b:1883\n  username: u\n  password_path: "+unreadable+"\n"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mqtt.password_path")
	assert.Contains(t, err.Error(), unreadable)
}

func TestCheckRenderedRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_key_ever")
}

func TestCheckRenderedRejectsOverlappingCredentials(t *testing.T) {
	t.Parallel()
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("t"), 0o600))
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\ngit_token_path: "+token+"\nwebhook_secret_path: "+token+"\n"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same credential file")
}

func TestCheckRenderedRejectsMissingRepoURL(t *testing.T) {
	t.Parallel()
	err := CheckRenderedConfig([]byte("hostname: node-1\n"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo_url is required")
}

// CheckRenderedConfig reads each credential file at the path Locate gives
// it, names both paths when they differ, and refuses a config Locate
// refuses.
func TestCheckRenderedConfigLocate(t *testing.T) {
	t.Parallel()
	seen := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seen, "git.token"), []byte("secret\n"), 0o600))
	data := []byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\ngit_token_path: /next/git.token\nwebhook_secret_path: /next/hook\n")
	relocate := func(cred Credential) (string, error) {
		return filepath.Join(seen, filepath.Base(cred.Path)), nil
	}

	err := CheckRenderedConfig(data, relocate)
	require.Error(t, err)
	require.ErrorContains(t, err, "webhook_secret_path (/next/hook, read at "+filepath.Join(seen, "hook")+") is not readable")
	assert.NotContains(t, err.Error(), "git_token_path", "git.token is read where Locate puts it")

	// The overlap check compares the files where they are read.
	require.NoError(t, os.Link(filepath.Join(seen, "git.token"), filepath.Join(seen, "hook")))
	err = CheckRenderedConfig(data, relocate)
	require.ErrorContains(t, err, "name the same credential file")

	err = CheckRenderedConfig(data, func(Credential) (string, error) { return "", errors.New("not mounted") })
	require.ErrorContains(t, err, "credential git_token_path (/next/git.token): not mounted")
}
