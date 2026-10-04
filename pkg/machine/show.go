package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// Environment is where bootstrap machine runs.
type Environment struct {
	GOOS        string
	InContainer bool
	// Root: the process runs as root.
	Root bool
	// Podman: the podman binary is installed.
	Podman bool
	// PassCLI: the pass-cli binary is on PATH.
	PassCLI bool
}

func (e Environment) check() error {
	if e.GOOS != "linux" {
		return fmt.Errorf("bootstrap machine needs Linux: it inspects the Machine's users, systemd and Podman (running on %s)", e.GOOS)
	}
	if e.InContainer {
		return errors.New("bootstrap machine must run on the Machine itself, not inside a container: " +
			"it inspects the Machine's users, systemd and Podman")
	}
	return nil
}

// checkRun is check plus what a run needs beyond a plan.
func (e Environment) checkRun() error {
	if err := e.check(); err != nil {
		return err
	}
	if !e.Root {
		return errors.New("bootstrap machine must run as root: it creates users and directories and enables services " +
			"(--plan previews the steps without root)")
	}
	if !e.Podman {
		return errors.New("podman is not installed (no podman on PATH): install it first, bootstrap machine installs no packages")
	}
	return nil
}

// Config is the input of ShowPlan and Run.
type Config struct {
	Machine string
	// RepoDir is the Fleet checkout on the Machine.
	RepoDir string
	// SecretsDir is the operator's --secrets-dir, <dir>/<hostname>/<file>;
	// empty when not given.
	SecretsDir string
	// OnePasswordTokenFile and ProtonPassPATFile hold the Machine's provider
	// token, at most one of them; empty when not given.
	OnePasswordTokenFile string
	ProtonPassPATFile    string
	// Providers opens the provider of the token to resolve the Hosts'
	// bootstrap: references.
	Providers Providers
	Env       Environment
	Stdout    io.Writer
}

// ShowPlan prints what bootstrapping cfg.Machine would do, checking every
// step through the read side of ops. It writes nothing to the Machine and
// never needs root: checks root alone can perform show as unknown.
func ShowPlan(ctx context.Context, cfg Config, ops HostOps) error {
	if err := cfg.Env.check(); err != nil {
		return err
	}
	plan, err := load(ctx, cfg)
	if err != nil {
		return err
	}
	results, err := Evaluate(ctx, plan, ops)
	if err != nil {
		return err
	}
	return Render(cfg.Stdout, plan, results)
}

// load plans cfg.Machine from the Fleet checked out at cfg.RepoDir, with the
// credential files of cfg.SecretsDir, the provider token and the values of
// the Hosts' bootstrap: references, resolved before load returns.
func load(ctx context.Context, cfg Config) (*Plan, error) {
	prov, tokenFile, err := cfg.tokenFlag()
	if err != nil {
		return nil, err
	}
	if cfg.RepoDir == "" {
		return nil, errors.New("--repo-dir is required: the Fleet checkout on this Machine")
	}
	// The readability check walks the real tree, not a symlink to it.
	repoDir, err := realPath(cfg.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("resolving --repo-dir: %w", err)
	}
	repo, err := config.OpenRepo(repoDir)
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	opts := Options{RepoDir: repoDir}
	if cfg.SecretsDir != "" {
		root, err := openSecretsDir(repoDir, cfg.SecretsDir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		opts.SecretsDir = &SecretsDir{Path: root.Name(), FS: root.FS()}
	}
	if opts.Token, err = readToken(repoDir, prov, tokenFile); err != nil {
		return nil, err
	}
	hosts, err := machineHosts(repo.Config, cfg.Machine)
	if err != nil {
		return nil, err
	}
	if opts.Refs, err = resolveRefs(ctx, cfg.Providers, opts.Token, hosts); err != nil {
		return nil, err
	}
	return New(repo.Config, cfg.Machine, opts)
}

// openSecretsDir opens the --secrets-dir p as an os.Root: a symlink below
// it cannot read, as root, a file outside it into a Host's secrets.
func openSecretsDir(repoDir, p string) (*os.Root, error) {
	dir, err := outsideCheckout(repoDir, p, "--secrets-dir")
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening --secrets-dir: %w", err)
	}
	return root, nil
}

// outsideCheckout is p resolved, which must not lie at or below the Fleet
// checkout repoDir: the setup makes the checkout readable by every user,
// credentials included.
func outsideCheckout(repoDir, p, flag string) (string, error) {
	resolved, err := realPath(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", flag, err)
	}
	if rel, err := filepath.Rel(repoDir, resolved); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return "", fmt.Errorf("%s must not lie inside the Fleet checkout %s: "+
			"bootstrap makes the checkout readable by every user", flag, repoDir)
	}
	return resolved, nil
}

// readToken reads the operator's token file of prov; nil when the operator
// gave no provider token.
func readToken(repoDir string, prov *provider, file string) (*ProviderToken, error) {
	if prov == nil {
		return nil, nil //nolint:nilnil // no token is not an error
	}
	p, err := outsideCheckout(repoDir, file, prov.flag)
	if err != nil {
		return nil, err
	}
	content, err := readCredential(os.DirFS("/"), strings.TrimPrefix(p, "/"))
	if err != nil {
		return nil, fmt.Errorf("reading %s %s: %w", prov.flag, p, err)
	}
	return &ProviderToken{provider: prov, Path: p, Content: content}, nil
}

// realPath is p absolute, with symlinks resolved.
func realPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}
