package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	Env        Environment
	Stdout     io.Writer
}

// ShowPlan prints what bootstrapping cfg.Machine would do, checking every
// step through the read side of ops. It writes nothing to the Machine and
// never needs root: checks root alone can perform show as unknown.
func ShowPlan(ctx context.Context, cfg Config, ops HostOps) error {
	if err := cfg.Env.check(); err != nil {
		return err
	}
	plan, err := load(cfg)
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
// credential files of cfg.SecretsDir.
func load(cfg Config) (*Plan, error) {
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
		dir, err := realPath(cfg.SecretsDir)
		if err != nil {
			return nil, fmt.Errorf("resolving --secrets-dir: %w", err)
		}
		// The setup makes the checkout readable by every user.
		if rel, err := filepath.Rel(repoDir, dir); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return nil, fmt.Errorf("--secrets-dir must not lie inside the Fleet checkout %s: "+
				"bootstrap makes the checkout readable by every user", repoDir)
		}
		// An os.Root: a symlink below the directory cannot read, as root,
		// a file outside it into a Host's secrets.
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, fmt.Errorf("opening --secrets-dir: %w", err)
		}
		defer root.Close()
		opts.SecretsDir = &SecretsDir{Path: dir, FS: root.FS()}
	}
	return New(repo.Config, cfg.Machine, opts)
}

// realPath is p absolute, with symlinks resolved.
func realPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}
