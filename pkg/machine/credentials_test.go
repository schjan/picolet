package machine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

// secretsDir writes files (path below the directory → content) into a fresh
// --secrets-dir and returns its path.
func secretsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return dir
}

// operatorSecrets holds files for both rootless Hosts of vps-1, one of them
// below a subdirectory, and none for the rootful vps-1-system.
var operatorSecrets = map[string]string{
	"vps-1/git-token":        "pi-token",
	"vps-1/github/app.pem":   "pi-app-key",
	"vps-1-runner/git-token": "runner-token",
	"other-host/git-token":   "not on this Machine",
	"README":                 "outside every Host",
}

// runWithSecrets bootstraps vps-1 as root with --secrets-dir dir and returns
// the output with the checkout and secrets paths replaced.
func runWithSecrets(t *testing.T, dir string, ops machine.HostOps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine:    "vps-1",
		RepoDir:    exampleFleet,
		SecretsDir: dir,
		Env:        rootOnLinux(),
		Stdout:     &out,
	}, ops)
	return redact(t, out.String(), dir), err
}

func redact(t *testing.T, out, secrets string) string {
	t.Helper()
	out = strings.ReplaceAll(out, exampleFleetAbs(t), "<repo>")
	resolved, err := filepath.EvalSymlinks(secrets)
	require.NoError(t, err)
	return strings.ReplaceAll(out, resolved, "<secrets>")
}

// expectBootstrappedMachine sets ops up as a Machine whose setup phase is
// done.
func expectBootstrappedMachine(t *testing.T, ops *mocks.MockHostOps) {
	t.Helper()
	expectBootstrappedUser(ops, pi, running)
	expectBootstrappedUser(ops, runner, running)
	expectBootstrappedRootful(ops, running)
	ops.EXPECT().WorldReadableTree(exampleFleetAbs(t)).Return(true, nil)
}

// secretFile is a credential file as placed: regular, owned by u, 0600.
func secretFile(u machine.User) machine.PathInfo {
	return machine.PathInfo{Exists: true, UID: u.UID, GID: u.GID, Mode: 0o600}
}

// Every file below <dir>/<host>/ lands in the Host's secrets directory,
// owned by the Host's user, mode 0600, a subdirectory created private. The
// rootful Host has no directory below <dir>: a warning, and the run goes on.
// A new file needs an Agent restart too: a running Agent has not read it.
func TestRunPlacesSecretsDirFiles(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	expectAgentsStarted(t, ops)
	ops.EXPECT().RestartUserUnit(mock.Anything, pi, "picolet.service").Return(nil).Once()
	ops.EXPECT().RestartUserUnit(mock.Anything, runner, "picolet.service").Return(nil).Once()
	piOwner, runnerOwner := machine.Owner{UID: 1000, GID: 1000}, machine.Owner{UID: 1001, GID: 1001}
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/git-token", []byte("pi-token"), piOwner, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/github").Return(machine.PathInfo{}, nil)
	ops.EXPECT().EnsureDir("/home/pi", ".config/picolet/secrets/github", piOwner, os.FileMode(0o700)).Return(nil).Once()
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/github/app.pem").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/github/app.pem", []byte("pi-app-key"), piOwner, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().Stat("/home/runner/.config/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/runner", ".config/picolet/secrets/git-token", []byte("runner-token"), runnerOwner, os.FileMode(0o600)).
		Return(nil).Once()

	out, err := runWithSecrets(t, secretsDir(t, operatorSecrets), ops)
	require.NoError(t, err)
	goldie.New(t).Assert(t, "run-secrets-dir", []byte(out))
}

// The rootful Host's files go to /etc/picolet/secrets, owned by root; every
// directory between it and a nested file is created private.
func TestRunPlacesRootfulSecretsDirFiles(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	expectAgentsStarted(t, ops)
	ops.EXPECT().RestartSystemUnit(mock.Anything, "picolet-system.service").Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/git-token", []byte("system-token"), machine.Owner{}, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/mqtt").Return(machine.PathInfo{}, nil)
	ops.EXPECT().EnsureDir("/", "etc/picolet/secrets/mqtt", machine.Owner{}, os.FileMode(0o700)).Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/mqtt/tls").Return(machine.PathInfo{}, nil)
	ops.EXPECT().EnsureDir("/", "etc/picolet/secrets/mqtt/tls", machine.Owner{}, os.FileMode(0o700)).Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/mqtt/tls/key.pem").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/mqtt/tls/key.pem", []byte("tls-key"), machine.Owner{}, os.FileMode(0o600)).
		Return(nil).Once()

	out, err := runWithSecrets(t, secretsDir(t, map[string]string{
		"vps-1-system/git-token":        "system-token",
		"vps-1-system/mqtt/tls/key.pem": "tls-key",
	}), ops)
	require.NoError(t, err)
	require.Regexp(t, `applied\s+vps-1-system/credential/git-token\s+.*`+
		`wrote /etc/picolet/secrets/git-token, Agent restart required\n`, out)
	require.Contains(t, out, "warning: no credential files for vps-1: <secrets>/vps-1 does not exist\n")
	require.Contains(t, out, "warning: no credential files for vps-1-runner: <secrets>/vps-1-runner does not exist\n")
	require.Regexp(t, `\nvps-1-system\s+root\s+9418\s+healthy\s+restarted \(credential files written\)\n`, out)
	require.NotContains(t, out, "system-token")
}

// A run interrupted just after writing a credential file still reports the
// Host's Agent as needing a restart: the next run finds the file current and
// could not tell.
func TestRunInterruptedAfterCredentialWriteReportsRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	ops.EXPECT().Stat("/etc/picolet/secrets/git-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/git-token", []byte("system-token"), machine.Owner{}, os.FileMode(0o600)).
		RunAndReturn(func(string, string, []byte, machine.Owner, os.FileMode) error {
			cancel()
			return nil
		}).Once()
	dir := secretsDir(t, map[string]string{"vps-1-system/git-token": "system-token"})

	var out bytes.Buffer
	err := machine.Run(ctx, machine.Config{
		Machine: "vps-1", RepoDir: exampleFleet, SecretsDir: dir, Env: rootOnLinux(), Stdout: &out,
	}, ops)
	require.ErrorIs(t, err, context.Canceled)
	require.Regexp(t, `applied\s+vps-1-system/credential/git-token\s`, out.String())
	require.Regexp(t, `\nvps-1-system\s+root\s+9418\s+not checked\s+required \(credential files written\)\n`, out.String())
}

// expectPlacedSecrets sets ops up as a Machine holding the credential files
// of operatorSecrets as a previous run left them, but for three: runner's
// git-token holds an older token, pi's app.pem has the right content with a
// mode someone loosened, and so has its github directory.
func expectPlacedSecrets(ops *mocks.MockHostOps) {
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/git-token").Return(secretFile(pi), nil)
	ops.EXPECT().FileContentEquals("/home/pi/.config/picolet/secrets/git-token", []byte("pi-token")).Return(true, nil)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/github").Return(dir(1000, 1000, 0o755), nil)
	loosened := secretFile(pi)
	loosened.Mode = 0o644
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/github/app.pem").Return(loosened, nil)
	ops.EXPECT().FileContentEquals("/home/pi/.config/picolet/secrets/github/app.pem", []byte("pi-app-key")).Return(true, nil)
	ops.EXPECT().Stat("/home/runner/.config/picolet/secrets/git-token").Return(secretFile(runner), nil)
	ops.EXPECT().FileContentEquals("/home/runner/.config/picolet/secrets/git-token", []byte("runner-token")).Return(false, nil)
}

// A re-run leaves a current file alone, rewrites a changed one and marks its
// Host for an Agent restart; a file whose content is current but whose mode
// is not gets the mode in place, neither rewritten nor restarting its Agent,
// and so does its directory.
func TestRunRewritesChangedSecretsDirFiles(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	expectPlacedSecrets(ops)
	expectAgentsStarted(t, ops)
	ops.EXPECT().RestartUserUnit(mock.Anything, runner, "picolet.service").Return(nil).Once()
	piOwner := machine.Owner{UID: 1000, GID: 1000}
	ops.EXPECT().EnsureDir("/home/pi", ".config/picolet/secrets/github", piOwner, os.FileMode(0o700)).Return(nil).Once()
	ops.EXPECT().SetOwnerMode("/home/pi", ".config/picolet/secrets/github/app.pem", piOwner, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().WriteFile("/home/runner", ".config/picolet/secrets/git-token", []byte("runner-token"),
		machine.Owner{UID: 1001, GID: 1001}, os.FileMode(0o600)).Return(nil).Once()

	out, err := runWithSecrets(t, secretsDir(t, operatorSecrets), ops)
	require.NoError(t, err)
	require.Regexp(t, `already done\s+vps-1/credential/git-token\s`, out)
	require.Regexp(t, `applied\s+vps-1/credential-dir/github\s`, out)
	require.Regexp(t, `applied\s+vps-1/credential/github/app.pem\s.*set owner and mode of /home/pi/.config/picolet/secrets/github/app.pem\n`, out)
	require.Regexp(t, `applied\s+vps-1-runner/credential/git-token\s.*`+
		`wrote /home/runner/.config/picolet/secrets/git-token, Agent restart required\n`, out)
	require.Contains(t, out, "\n6 applied, 26 already done\n")
	require.Regexp(t, `\nvps-1-runner\s+runner\s+9419\s+healthy\s+restarted \(credential files written\)\n`, out)
	require.Regexp(t, `\nvps-1\s+pi\s+9417\s+healthy\s+not needed\n`, out)
}

// A --secrets-dir at or below the Fleet checkout is refused: making the
// checkout readable by every user would make the credentials readable too.
func TestRejectsSecretsDirInsideCheckout(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	require.NoError(t, os.CopyFS(repo, os.DirFS(exampleFleet)))
	secrets := filepath.Join(repo, "operator-secrets")
	require.NoError(t, os.MkdirAll(filepath.Join(secrets, "vps-1"), 0o700))

	for _, dir := range []string{secrets, repo} {
		var out bytes.Buffer
		err := machine.Run(context.Background(), machine.Config{
			Machine: "vps-1", RepoDir: repo, SecretsDir: dir, Env: rootOnLinux(), Stdout: &out,
		}, mocks.NewMockHostOps(t))
		require.ErrorContains(t, err, "--secrets-dir must not lie inside the Fleet checkout", dir)
		require.Empty(t, out.String())
	}
}

// The plan shows each credential file as would do or already done, why, and
// never a value, neither the file's nor what the Machine holds.
func TestShowPlanSecretsDir(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	expectPlacedSecrets(ops)
	dir := secretsDir(t, operatorSecrets)

	var buf bytes.Buffer
	err := machine.ShowPlan(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: exampleFleet, SecretsDir: dir, Env: linuxHost(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	out := redact(t, buf.String(), dir)
	for _, value := range operatorSecrets {
		require.NotContains(t, out, value)
	}
	goldie.New(t).Assert(t, "plan-secrets-dir", []byte(out))
}

// A --secrets-dir that cannot be read as planned stops the command before
// its first step: the mock has no expectations.
func TestRejectsSecretsDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		want  string
	}{
		{
			name:  "missing",
			setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, "missing") },
			want:  "resolving --secrets-dir",
		},
		{
			name: "Host entry not a directory",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "vps-1"), []byte("x"), 0o600))
				return dir
			},
			want: "/vps-1 is not a directory",
		},
		{
			name: "symlink out of the directory",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				outside := filepath.Join(dir, "outside")
				require.NoError(t, os.WriteFile(outside, []byte("host file"), 0o600))
				secrets := filepath.Join(dir, "secrets")
				require.NoError(t, os.MkdirAll(filepath.Join(secrets, "vps-1"), 0o700))
				require.NoError(t, os.Symlink(outside, filepath.Join(secrets, "vps-1", "git-token")))
				return secrets
			},
			want: "vps-1/git-token",
		},
		{
			name: "not a regular file",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "vps-1"), 0o700))
				require.NoError(t, os.Symlink(".", filepath.Join(dir, "vps-1", "loop")))
				return dir
			},
			want: "vps-1/loop: not a regular file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := machine.Run(context.Background(), machine.Config{
				Machine: "vps-1", RepoDir: exampleFleet, SecretsDir: tt.setup(t, t.TempDir()), Env: rootOnLinux(), Stdout: &out,
			}, mocks.NewMockHostOps(t))
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out.String(), "a rejected run prints nothing")
		})
	}
}
