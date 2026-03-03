package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

var version = "dev"

//nolint:funlen // declarative CLI registration; splitting reduces readability
func main() {
	app := &cli.Command{
		Name:    "picolet",
		Usage:   "GitOps agent for Podman Quadlets",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "repo-dir",
				Value:   ".",
				Usage:   "path to repository root",
				Sources: cli.EnvVars("PICOLET_REPO_DIR"),
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "enable debug logging",
				Sources: cli.EnvVars("PICOLET_VERBOSE"),
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			level := slog.LevelInfo
			if cmd.Bool("verbose") {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: level,
			})))
			return ctx, nil
		},
		Commands: []*cli.Command{
			{
				Name:  "validate",
				Usage: "validate all quadlet files and manifests for every host",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runValidate(ctx, cmd.Root().String("repo-dir"))
				},
			},
			{
				Name:  "resolve",
				Usage: "render templates for a host and print desired state",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "host",
						Usage:    "hostname to resolve",
						Required: true,
					},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return runResolve(cmd.Root().String("repo-dir"), cmd.String("host"))
				},
			},
			{
				Name:  "run",
				Usage: "start the reconciliation loop",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "config",
						Value:   "/etc/picolet/config.yml",
						Usage:   "agent config file",
						Sources: cli.EnvVars("PICOLET_CONFIG"),
					},
				},
				Before: jsonLogging,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runAgent(ctx, cmd.String("config"), false)
				},
			},
			{
				Name:  "dry-run",
				Usage: "simulate reconciliation without applying changes",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "host",
						Usage:    "hostname to simulate",
						Required: true,
					},
					&cli.StringFlag{
						Name:    "repo-dir",
						Value:   ".",
						Usage:   "path to repository root",
						Sources: cli.EnvVars("PICOLET_REPO_DIR"),
					},
				},
				Before: jsonLogging,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runDryRun(ctx, cmd.String("repo-dir"), cmd.String("host"))
				},
			},
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := app.Run(ctx, os.Args); err != nil {
		slog.Error("command failed", "error", err)
		cancel()
		os.Exit(1)
	}
	cancel()
}

// jsonLogging switches the default logger to JSON for daemon subcommands.
func jsonLogging(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	level := slog.LevelInfo
	if cmd.Root().Bool("verbose") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
	return ctx, nil
}

func runAgent(ctx context.Context, configPath string, dryRun bool) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}

	metrics.Register()

	// Connect to systemd D-Bus
	systemd, err := applier.NewDBusSystemdManager(ctx)
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	// Connect to Podman socket
	podman, err := applier.NewSocketPodmanClient(ctx, "/run/podman/podman.sock")
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}

	opts := []agent.Option{
		agent.WithDryRun(dryRun),
		agent.WithSystemd(systemd),
		agent.WithPodman(podman),
		agent.WithFileWriter(applier.NewAtomicFileWriter()),
	}

	a := agent.New(cfg, opts...)
	return a.Run(ctx)
}

func runDryRun(ctx context.Context, repoDir, hostname string) error {
	repoFS := os.DirFS(repoDir)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	r := resolver.New(repoFS, cfg, nil)
	resolved, err := r.ResolveHost(hostname)
	if err != nil {
		return err
	}

	// Validate first
	v := validator.New()
	if err := v.ValidateAll(ctx, r, cfg); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Diff against disk state (or empty state for fresh host)
	store := state.NewStore("/var/lib/picolet/state.json")
	st, err := store.Load()
	if err != nil {
		slog.Warn("could not load state, using empty state", "error", err)
		st = &state.State{ManagedFiles: make(map[string]string)}
	}

	rec := reconciler.New()
	changeset := rec.Diff(resolved.Files, st, nil)

	if !changeset.HasChanges() {
		slog.Info("no changes detected")
		return nil
	}

	slog.Info("dry-run changes",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	for _, c := range changeset.Changes {
		if c.Action == reconciler.ActionNoop {
			continue
		}
		slog.Info("change",
			"action", c.Action,
			"path", c.DestPath,
			"category", c.Category,
		)
	}

	return nil
}

func runValidate(ctx context.Context, repoDir string) error {
	repoFS := os.DirFS(repoDir)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	r := resolver.New(repoFS, cfg, nil)
	v := validator.New()
	return v.ValidateAll(ctx, r, cfg)
}

func runResolve(repoDir, host string) error {
	cfg, err := config.LoadAll(os.DirFS(repoDir))
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	r := resolver.New(os.DirFS(repoDir), cfg, nil)
	resolved, err := r.ResolveHost(host)
	if err != nil {
		return err
	}
	for _, f := range resolved.Files {
		fmt.Printf("=== %s ===\n%s\n", f.DestPath, f.Content)
	}
	return nil
}
