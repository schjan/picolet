package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/schjan/picolet/pkg/config"
)

// Environment is where bootstrap machine runs.
type Environment struct {
	GOOS        string
	InContainer bool
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

// PlanConfig is the input of ShowPlan.
type PlanConfig struct {
	Machine string
	// RepoDir is the Fleet checkout on the Machine.
	RepoDir string
	Env     Environment
	Stdout  io.Writer
}

// ShowPlan prints what bootstrapping cfg.Machine would do, checking every
// step through the read side of ops. It writes nothing to the Machine and
// never needs root: checks root alone can perform show as unknown.
func ShowPlan(ctx context.Context, cfg PlanConfig, ops HostOps) error {
	if err := cfg.Env.check(); err != nil {
		return err
	}
	if cfg.RepoDir == "" {
		return errors.New("--repo-dir is required: the Fleet checkout on this Machine")
	}
	// The readability check walks the real tree, not a symlink to it.
	repoDir, err := filepath.Abs(cfg.RepoDir)
	if err == nil {
		repoDir, err = filepath.EvalSymlinks(repoDir)
	}
	if err != nil {
		return fmt.Errorf("resolving --repo-dir: %w", err)
	}
	repo, err := config.OpenRepo(repoDir)
	if err != nil {
		return err
	}
	defer repo.Close()

	plan, err := New(repo.Config, cfg.Machine, Options{RepoDir: repoDir})
	if err != nil {
		return err
	}
	results, err := Evaluate(ctx, plan, ops)
	if err != nil {
		return err
	}
	return Render(cfg.Stdout, plan, results)
}
