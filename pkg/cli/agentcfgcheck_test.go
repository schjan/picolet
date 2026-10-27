package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
)

// credentialContent is the content of every credential file the tests
// write; it must never reach the check's output.
const credentialContent = "s3cr3t-credential-content"

func writeCheckFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// writeCheckConfig writes an Agent config naming the given credential paths
// and returns its path.
func writeCheckConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yml")
	writeCheckFile(t, path, "hostname: node-1\nrepo_url: https://example.com/fleet.git\n"+body)
	return path
}

// secretKey is the AGE-SECRET-KEY line of the named TEST-ONLY identity.
func secretKey(t *testing.T, name string) string {
	t.Helper()
	for line := range strings.Lines(string(testsops.IdentityFile(t, name))) {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("identity %s holds no AGE-SECRET-KEY line", name)
	return ""
}

func TestAgentConfigCheckPassesCleanConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCheckFile(t, filepath.Join(dir, "git.token"), credentialContent)
	writeCheckFile(t, filepath.Join(dir, "age.key"), string(testsops.IdentityFile(t, "srv-1")))
	cfg := writeCheckConfig(t, dir, "git_token_path: "+filepath.Join(dir, "git.token")+
		"\nsops:\n  age_identity_path: "+filepath.Join(dir, "age.key")+"\n")

	var out bytes.Buffer
	require.NoError(t, runAgentConfigCheck(&out, cfg))
	assert.Equal(t, cfg+": ok\n", out.String())
}

// Every unreadable credential is reported, each by setting and path, not
// only the first the Agent would trip over.
func TestAgentConfigCheckReportsEveryUnreadableCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "git.token")
	unreadable := filepath.Join(dir, "mqtt.pw") // a directory: unreadable even for root
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	webhook := filepath.Join(dir, "webhook")
	writeCheckFile(t, webhook, credentialContent)
	cfg := writeCheckConfig(t, dir, "git_token_path: "+missing+
		"\nwebhook_secret_path: "+webhook+
		"\nmqtt:\n  broker_url: tcp://b:1883\n  username: u\n  password_path: "+unreadable+"\n")

	var out bytes.Buffer
	err := runAgentConfigCheck(&out, cfg)
	require.Error(t, err)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 3, out.String())
	assert.Equal(t, cfg+": FAIL", lines[0])
	assert.Contains(t, lines[1], "git_token_path ("+missing+")")
	assert.Contains(t, lines[2], "mqtt.password_path ("+unreadable+")")
	assert.NotContains(t, out.String(), "webhook_secret_path")
	assert.NotContains(t, out.String(), credentialContent)
}

func TestAgentConfigCheckReportsDuplicatedCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shared := filepath.Join(dir, "token")
	writeCheckFile(t, shared, credentialContent)
	cfg := writeCheckConfig(t, dir, "git_token_path: "+shared+"\nwebhook_secret_path: "+shared+
		"\nonepassword:\n  token_path: "+shared+"\n")

	var out bytes.Buffer
	require.Error(t, runAgentConfigCheck(&out, cfg))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 4, out.String())
	assert.Equal(t, cfg+": FAIL", lines[0])
	for _, line := range lines[1:] {
		assert.Contains(t, line, "("+shared+")")
		assert.Contains(t, line, "name the same credential file")
	}
	assert.NotContains(t, out.String(), credentialContent)
}

// An age identity file that does not parse is reported without quoting it.
func TestAgentConfigCheckNeverPrintsIdentityContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	identity := filepath.Join(dir, "age.key")
	writeCheckFile(t, identity, string(testsops.IdentityFile(t, "srv-1"))+"\nnot an identity "+credentialContent+"\n")
	cfg := writeCheckConfig(t, dir, "sops:\n  age_identity_path: "+identity+"\n")

	var out bytes.Buffer
	require.Error(t, runAgentConfigCheck(&out, cfg))
	assert.Contains(t, out.String(), "sops.age_identity_path ("+identity+")")
	assert.NotContains(t, out.String(), credentialContent)
	assert.NotContains(t, out.String(), secretKey(t, "srv-1"))
}

// A malformed AGE-SECRET-KEY line is reported without quoting it: age's own
// parse error would.
func TestAgentConfigCheckNeverPrintsMalformedIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	identity := filepath.Join(dir, "age.key")
	key := secretKey(t, "srv-1")
	truncated := key[:len(key)-8]
	writeCheckFile(t, identity, truncated+"\n")
	cfg := writeCheckConfig(t, dir, "sops:\n  age_identity_path: "+identity+"\n")

	var out bytes.Buffer
	require.Error(t, runAgentConfigCheck(&out, cfg))
	assert.Contains(t, out.String(), "sops.age_identity_path ("+identity+")")
	assert.NotContains(t, out.String(), truncated[len("AGE-SECRET-KEY-1"):])
}

// A credential file inside protonpass.session_dir is named twice.
func TestAgentConfigCheckReportsCredentialInsideSessionDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	session := filepath.Join(dir, "session")
	require.NoError(t, os.Mkdir(session, 0o700))
	token := filepath.Join(session, "git.token")
	writeCheckFile(t, token, credentialContent)
	cfg := writeCheckConfig(t, dir, "git_token_path: "+token+"\nprotonpass:\n  session_dir: "+session+"\n")

	var out bytes.Buffer
	require.Error(t, runAgentConfigCheck(&out, cfg))
	assert.Contains(t, out.String(), "git_token_path ("+token+")")
	assert.Contains(t, out.String(), "protonpass.session_dir ("+session+")")
	assert.NotContains(t, out.String(), credentialContent)
}

func TestAgentConfigCheckReportsUnloadableConfig(t *testing.T) {
	t.Parallel()
	cfg := writeCheckConfig(t, t.TempDir(), "no_such_key_ever: x\n")

	var out bytes.Buffer
	require.Error(t, runAgentConfigCheck(&out, cfg))
	assert.Contains(t, out.String(), "no_such_key_ever")
}

// picolet agentcfg check takes --config and fails the process on a failed
// check.
func TestAgentConfigCheckCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := writeCheckConfig(t, dir, "git_token_path: "+filepath.Join(dir, "absent")+"\n")

	app := newApp()
	app.Before = nil // keep the global slog default of parallel tests
	var out bytes.Buffer
	app.Writer = &out
	err := app.Run(t.Context(), []string{"picolet", "agentcfg", "check", "--config", cfg})
	require.ErrorIs(t, err, errAgentConfigCheck)
	assert.Contains(t, out.String(), "git_token_path ("+filepath.Join(dir, "absent")+")")
}
