package machine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/machine"
	"github.com/schjan/picolet/pkg/machine"
)

// tokenFile writes the operator's provider token to a fresh file outside the
// Fleet checkout and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "machine-token")
	require.NoError(t, os.WriteFile(p, []byte(token), 0o600))
	return p
}

// unusedProviders fails the test when a provider is opened: no Host needs
// one of its references resolved.
func unusedProviders(t *testing.T) machine.Providers {
	t.Helper()
	return machine.Providers{
		OnePassword: func(context.Context, string) (machine.RefReader, error) {
			t.Error("1Password opened without a reference to resolve")
			return nil, nil
		},
		ProtonPass: func(context.Context, string, string) (machine.RefReader, error) {
			t.Error("Proton Pass opened without a reference to resolve")
			return nil, nil
		},
	}
}

// The Machine's provider token lands in every Host's secrets directory like
// a --secrets-dir file, beside the --secrets-dir files: the sources add up.
// No Host has a reference to resolve, so no provider is opened.
func TestRunPlacesProviderToken(t *testing.T) {
	t.Parallel()
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachine(t, ops)
	piOwner, runnerOwner := machine.Owner{UID: 1000, GID: 1000}, machine.Owner{UID: 1001, GID: 1001}
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/git-token").Return(secretFile(pi), nil)
	ops.EXPECT().FileContentEquals("/home/pi/.config/picolet/secrets/git-token", []byte("pi-token")).Return(true, nil)
	for _, p := range []string{
		"/home/pi/.config/picolet/secrets/op-service-account-token",
		"/home/runner/.config/picolet/secrets/op-service-account-token",
		"/etc/picolet/secrets/op-service-account-token",
	} {
		ops.EXPECT().Stat(p).Return(machine.PathInfo{}, nil)
	}
	token := []byte("ops_machine-token\n")
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/op-service-account-token", token, piOwner, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().WriteFile("/home/runner", ".config/picolet/secrets/op-service-account-token", token, runnerOwner, os.FileMode(0o600)).
		Return(nil).Once()
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/op-service-account-token", token, machine.Owner{}, os.FileMode(0o600)).
		Return(nil).Once()

	secrets := secretsDir(t, map[string]string{"vps-1/git-token": "pi-token"})
	opToken := tokenFile(t, string(token))
	var buf bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: exampleFleet, SecretsDir: secrets, OnePasswordTokenFile: opToken,
		Providers: unusedProviders(t), Env: rootOnLinux(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	out := redactPaths(t, buf.String(), map[string]string{exampleFleet: "<repo>", secrets: "<secrets>", opToken: "<token>"})
	require.NotContains(t, out, "ops_machine-token")
	goldie.New(t).Assert(t, "run-provider-token", []byte(out))
}

// redactPaths replaces each path, resolved, by its placeholder.
func redactPaths(t *testing.T, out string, paths map[string]string) string {
	t.Helper()
	for p, placeholder := range paths {
		abs, err := filepath.Abs(p)
		require.NoError(t, err)
		resolved, err := filepath.EvalSymlinks(abs)
		require.NoError(t, err)
		out = strings.ReplaceAll(out, resolved, placeholder)
	}
	return out
}

// fleetWithBootstrap copies the example fleet and appends a bootstrap:
// block to the host.yml of each Host in blocks, returning the copy's
// resolved path.
func fleetWithBootstrap(t *testing.T, blocks map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.CopyFS(repo, os.DirFS(exampleFleet)))
	for host, block := range blocks {
		p := filepath.Join(repo, "hosts", host, "host.yml")
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString("bootstrap:\n" + block)
		require.NoError(t, errors.Join(err, f.Close()))
	}
	repo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return repo
}

// ciBlocks keeps vps-1-runner and vps-1-system off the provider: their
// files come from references, one shared, one the fake 1Password does not
// resolve and one of Proton Pass. vps-1 keeps the provider.
var ciBlocks = map[string]string{
	"vps-1-runner": "  git_token: op://ci/git/token\n  forge_token: op://ci/forge/token\n  mqtt_password: pass://ci/mqtt/password\n",
	"vps-1-system": "  git_token: op://ci/git/token\n",
}

// fakeOnePassword resolves values and records every batch it is asked for.
type fakeOnePassword struct {
	values    map[string]string
	tokenFile string
	batches   [][]string
}

func (f *fakeOnePassword) providers(t *testing.T) machine.Providers {
	t.Helper()
	p := unusedProviders(t)
	p.OnePassword = func(_ context.Context, tokenFile string) (machine.RefReader, error) {
		f.tokenFile = tokenFile
		return func(_ context.Context, refs []string) (map[string]string, error) {
			f.batches = append(f.batches, refs)
			var errs []error
			out := map[string]string{}
			for _, ref := range refs {
				if v, ok := f.values[ref]; ok {
					out[ref] = v
				} else {
					errs = append(errs, fmt.Errorf("resolving 1password secret %q: NotFound", ref))
				}
			}
			return out, errors.Join(errs...)
		}, nil
	}
	return p
}

// expectBootstrappedMachineAt is expectBootstrappedMachine for the Fleet
// checked out at repo.
func expectBootstrappedMachineAt(ops *mocks.MockHostOps, repo string) {
	expectBootstrappedUser(ops, pi, running)
	expectBootstrappedUser(ops, runner, running)
	expectBootstrappedRootful(ops, running)
	ops.EXPECT().WorldReadableTree(repo).Return(true, nil)
}

// A Host with bootstrap: files gets them from their references, resolved in
// one batch with the operator's token, owned by the Host's user, mode 0600,
// and never the Machine's token. A reference left unresolved, or of a
// provider the operator gave no token for, is a warning and a summary entry;
// the run goes on.
func TestRunPlacesBootstrapRefs(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, ciBlocks)
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachineAt(ops, repo)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/op-service-account-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/op-service-account-token", []byte("ops_machine-token"),
		machine.Owner{UID: 1000, GID: 1000}, os.FileMode(0o600)).Return(nil).Once()
	ops.EXPECT().Stat("/home/runner/.config/picolet/secrets/git_token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/runner", ".config/picolet/secrets/git_token", []byte("ghp_ci"),
		machine.Owner{UID: 1001, GID: 1001}, os.FileMode(0o600)).Return(nil).Once()
	ops.EXPECT().Stat("/etc/picolet/secrets/git_token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/", "etc/picolet/secrets/git_token", []byte("ghp_ci"), machine.Owner{}, os.FileMode(0o600)).
		Return(nil).Once()

	op := &fakeOnePassword{values: map[string]string{"op://ci/git/token": "ghp_ci"}}
	opToken := tokenFile(t, "ops_machine-token")
	var buf bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: repo, OnePasswordTokenFile: opToken,
		Providers: op.providers(t), Env: rootOnLinux(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	out := buf.String()

	require.Equal(t, [][]string{{"op://ci/forge/token", "op://ci/git/token"}}, op.batches, "one batch, each reference once")
	resolvedToken, err := filepath.EvalSymlinks(opToken)
	require.NoError(t, err)
	require.Equal(t, resolvedToken, op.tokenFile)
	require.Contains(t, out, "warning: 1Password: resolving 1password secret \"op://ci/forge/token\": NotFound\n")
	require.Contains(t, out, "warning: vps-1-runner: forge_token not placed: op://ci/forge/token not resolved by 1Password\n")
	require.Contains(t, out, "warning: vps-1-runner: mqtt_password not placed: pass://ci/mqtt/password needs --protonpass-pat-file\n")
	require.Regexp(t, `applied\s+vps-1-runner/credential/git_token\s+.*, from op://ci/git/token\s+`+
		`wrote /home/runner/.config/picolet/secrets/git_token, Agent restart required\n`, out)
	require.Contains(t, out, "\nNot placed (Secret Reference not resolved): vps-1-runner/forge_token, vps-1-runner/mqtt_password\n")
	require.NotContains(t, out, "ghp_ci")
	require.NotContains(t, out, "ops_machine-token")
}

// The plan resolves the references to compare the files, and never prints
// a value.
func TestShowPlanBootstrapRefs(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, ciBlocks)
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachineAt(ops, repo)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/op-service-account-token").Return(secretFile(pi), nil)
	ops.EXPECT().FileContentEquals("/home/pi/.config/picolet/secrets/op-service-account-token", []byte("ops_machine-token")).
		Return(true, nil)
	ops.EXPECT().Stat("/home/runner/.config/picolet/secrets/git_token").Return(secretFile(runner), nil)
	ops.EXPECT().FileContentEquals("/home/runner/.config/picolet/secrets/git_token", []byte("ghp_ci")).Return(false, nil)
	ops.EXPECT().Stat("/etc/picolet/secrets/git_token").Return(machine.PathInfo{}, nil)

	op := &fakeOnePassword{values: map[string]string{"op://ci/git/token": "ghp_ci"}}
	opToken := tokenFile(t, "ops_machine-token")
	var buf bytes.Buffer
	err := machine.ShowPlan(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: repo, OnePasswordTokenFile: opToken,
		Providers: op.providers(t), Env: linuxHost(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	out := redactPaths(t, buf.String(), map[string]string{repo: "<repo>", opToken: "<token>"})
	require.NotContains(t, out, "ghp_ci")
	require.NotContains(t, out, "ops_machine-token")
	goldie.New(t).Assert(t, "plan-bootstrap-refs", []byte(out))
}

// Without a provider token nothing is resolved and no provider is opened:
// the Hosts' references are warnings, the run writes nothing for them.
func TestRunWithoutProviderSkipsBootstrapRefs(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, ciBlocks)
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachineAt(ops, repo)

	var buf bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: repo, Providers: unusedProviders(t), Env: rootOnLinux(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	out := buf.String()
	require.Contains(t, out, "warning: vps-1-runner: git_token not placed: op://ci/git/token needs --onepassword-token-file\n")
	require.Contains(t, out, "warning: vps-1-system: git_token not placed: op://ci/git/token needs --onepassword-token-file\n")
	require.Contains(t, out, "\nPhase 2: credential files\n  nothing to do\n")
	require.Contains(t, out, "Not placed (Secret Reference not resolved): "+
		"vps-1-runner/forge_token, vps-1-runner/git_token, vps-1-runner/mqtt_password, vps-1-system/git_token\n")
}

// A provider error is shown, but never the token or a value it resolved,
// whatever the provider puts into it.
func TestProviderErrorsRedactSecrets(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, ciBlocks)
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachineAt(ops, repo)
	for _, p := range []string{
		"/home/pi/.config/picolet/secrets/op-service-account-token",
		"/home/runner/.config/picolet/secrets/git_token",
		"/etc/picolet/secrets/git_token",
	} {
		ops.EXPECT().Stat(p).Return(machine.PathInfo{}, nil)
	}
	leaky := unusedProviders(t)
	leaky.OnePassword = func(context.Context, string) (machine.RefReader, error) {
		return func(context.Context, []string) (map[string]string, error) {
			return map[string]string{"op://ci/git/token": "ghp_ci"},
				errors.New("forge_token failed after ghp_ci with token ops_machine-token")
		}, nil
	}
	var buf bytes.Buffer
	err := machine.ShowPlan(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: repo, OnePasswordTokenFile: tokenFile(t, "ops_machine-token\n"),
		Providers: leaky, Env: linuxHost(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "warning: 1Password: forge_token failed after <redacted> with token <redacted>\n")
}

// An empty bootstrap: is a declared block: the Host runs without a
// provider and has nothing to place, so it gets no Machine token either.
func TestRunEmptyBootstrapKeepsHostOffProvider(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, map[string]string{"vps-1-runner": "  {}\n", "vps-1-system": "  {}\n"})
	ops := mocks.NewMockHostOps(t)
	expectBootstrappedMachineAt(ops, repo)
	ops.EXPECT().Stat("/home/pi/.config/picolet/secrets/op-service-account-token").Return(machine.PathInfo{}, nil)
	ops.EXPECT().WriteFile("/home/pi", ".config/picolet/secrets/op-service-account-token", []byte("ops_machine-token"),
		machine.Owner{UID: 1000, GID: 1000}, os.FileMode(0o600)).Return(nil).Once()

	var buf bytes.Buffer
	err := machine.Run(context.Background(), machine.Config{
		Machine: "vps-1", RepoDir: repo, OnePasswordTokenFile: tokenFile(t, "ops_machine-token"),
		Providers: unusedProviders(t), Env: rootOnLinux(), Stdout: &buf,
	}, ops)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "Agent restart required (credential files written): vps-1\n")
}

// protonPassOpener opens a fake Proton Pass that logs in like pass-cli, a
// session and local.key in the session directory, recording the directory;
// openErr fails it after the login wrote them.
func protonPassOpener(t *testing.T, sessionDir *string, openErr error) machine.Providers {
	t.Helper()
	p := unusedProviders(t)
	p.ProtonPass = func(_ context.Context, _, dir string) (machine.RefReader, error) {
		*sessionDir = dir
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the session directory is private")
		for _, name := range []string{"local.key", "session.json"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("session state"), 0o600))
		}
		if openErr != nil {
			return nil, openErr
		}
		return func(context.Context, []string) (map[string]string, error) {
			return map[string]string{"pass://ci/runner-git/token": "ghp_ci"}, nil
		}, nil
	}
	return p
}

// The Proton Pass session lives in a temporary directory that is gone when
// the command returns, whether the provider worked or failed: no session or
// local.key is left on the Machine.
func TestProtonPassSessionDirRemoved(t *testing.T) {
	t.Parallel()
	repo := fleetWithBootstrap(t, map[string]string{"vps-1-runner": "  git_token: pass://ci/runner-git/token\n"})
	pat := tokenFile(t, "pst_machine::key")
	cfg := func(p machine.Providers) machine.Config {
		env := linuxHost()
		env.PassCLI = true
		return machine.Config{Machine: "vps-1", RepoDir: repo, ProtonPassPATFile: pat, Providers: p, Env: env, Stdout: &bytes.Buffer{}}
	}

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		ops := mocks.NewMockHostOps(t)
		expectBootstrappedMachineAt(ops, repo)
		for _, p := range []string{"/home/pi/.config/picolet/secrets/pp-pat", "/etc/picolet/secrets/pp-pat", "/home/runner/.config/picolet/secrets/git_token"} {
			ops.EXPECT().Stat(p).Return(machine.PathInfo{}, nil)
		}
		var sessionDir string
		require.NoError(t, machine.ShowPlan(context.Background(), cfg(protonPassOpener(t, &sessionDir, nil)), ops))
		require.NotEmpty(t, sessionDir)
		require.NoDirExists(t, sessionDir)
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		var sessionDir string
		err := machine.ShowPlan(context.Background(), cfg(protonPassOpener(t, &sessionDir, errors.New("login failed"))), mocks.NewMockHostOps(t))
		require.ErrorContains(t, err, "opening Proton Pass with --protonpass-pat-file: login failed")
		require.NotEmpty(t, sessionDir)
		require.NoDirExists(t, sessionDir)
	})
}

// Credential sources that cannot be used as given stop the command before
// its first step: the mock has no expectations, and nothing is printed.
//
//nolint:funlen // table-driven coverage of every rejected source is clearer inline.
func TestRejectsCredentialSources(t *testing.T) {
	t.Parallel()
	ciRepo := fleetWithBootstrap(t, map[string]string{"vps-1-runner": "  git_token: op://ci/git/token\n"})
	badRef := fleetWithBootstrap(t, map[string]string{"vps-1-runner": "  git_token: op://ci/git\n"})
	token := tokenFile(t, "ops_machine-token")
	repoWithToken := fleetWithBootstrap(t, nil)
	tokenInRepo := filepath.Join(repoWithToken, "op-token")
	require.NoError(t, os.WriteFile(tokenInRepo, []byte("ops_machine-token"), 0o600))
	failing := unusedProviders(t)
	failing.OnePassword = func(context.Context, string) (machine.RefReader, error) {
		return nil, errors.New("invalid service account token")
	}
	tests := []struct {
		name string
		cfg  machine.Config
		want string
	}{
		{
			name: "two provider tokens",
			cfg:  machine.Config{RepoDir: exampleFleet, OnePasswordTokenFile: token, ProtonPassPATFile: token},
			want: "--onepassword-token-file and --protonpass-pat-file exclude each other",
		},
		{
			name: "pass-cli missing",
			cfg:  machine.Config{RepoDir: exampleFleet, ProtonPassPATFile: token},
			want: "pass-cli is not installed (no pass-cli on PATH): --protonpass-pat-file needs the Proton Pass CLI",
		},
		{
			name: "token file missing",
			cfg:  machine.Config{RepoDir: exampleFleet, OnePasswordTokenFile: filepath.Join(t.TempDir(), "absent")},
			want: "resolving --onepassword-token-file",
		},
		{
			name: "token file a directory",
			cfg:  machine.Config{RepoDir: exampleFleet, OnePasswordTokenFile: t.TempDir()},
			want: "not a regular file",
		},
		{
			name: "token file inside the checkout",
			cfg:  machine.Config{RepoDir: repoWithToken, OnePasswordTokenFile: tokenInRepo},
			want: "--onepassword-token-file must not lie inside the Fleet checkout",
		},
		{
			name: "token and --secrets-dir place one file",
			cfg: machine.Config{
				RepoDir: exampleFleet, OnePasswordTokenFile: token,
				SecretsDir: secretsDir(t, map[string]string{"vps-1/op-service-account-token": "other"}),
			},
			want: "vps-1: ~pi/.config/picolet/secrets/op-service-account-token comes from both --secrets-dir and --onepassword-token-file",
		},
		{
			// Found before the provider opens: a failing one never gets to.
			name: "reference and --secrets-dir place one file",
			cfg: machine.Config{
				RepoDir: ciRepo, OnePasswordTokenFile: token, Providers: failing,
				SecretsDir: secretsDir(t, map[string]string{"vps-1-runner/git_token": "other"}),
			},
			want: "vps-1-runner: ~runner/.config/picolet/secrets/git_token comes from both --secrets-dir and op://ci/git/token",
		},
		{
			name: "unresolved reference and --secrets-dir place one file",
			cfg: machine.Config{
				RepoDir: ciRepo, SecretsDir: secretsDir(t, map[string]string{"vps-1-runner/git_token": "other"}),
			},
			want: "vps-1-runner: ~runner/.config/picolet/secrets/git_token comes from both --secrets-dir and op://ci/git/token",
		},
		{
			name: "provider error naming the token",
			cfg: machine.Config{RepoDir: ciRepo, OnePasswordTokenFile: token, Providers: machine.Providers{
				OnePassword: func(context.Context, string) (machine.RefReader, error) {
					return nil, errors.New("token ops_machine-token rejected")
				},
			}},
			want: "opening 1Password with --onepassword-token-file: token <redacted> rejected",
		},
		{
			name: "malformed reference",
			cfg:  machine.Config{RepoDir: badRef},
			want: "host vps-1-runner: bootstrap: git_token is not a Secret Reference (op://vault/item/field or pass://share/item/field)",
		},
		{
			name: "provider fails to open",
			cfg:  machine.Config{RepoDir: ciRepo, OnePasswordTokenFile: token, Providers: failing},
			want: "opening 1Password with --onepassword-token-file: invalid service account token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			tt.cfg.Machine, tt.cfg.Env, tt.cfg.Stdout = "vps-1", rootOnLinux(), &out
			if tt.cfg.Providers.OnePassword == nil {
				tt.cfg.Providers = unusedProviders(t)
			}
			err := machine.Run(context.Background(), tt.cfg, mocks.NewMockHostOps(t))
			require.ErrorContains(t, err, tt.want)
			require.NotContains(t, err.Error(), "ops_machine-token")
			require.Empty(t, out.String(), "a rejected run prints nothing")
		})
	}
}
