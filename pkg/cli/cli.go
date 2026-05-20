// Package cli holds the picolet CLI command tree, action runners, and the
// process entrypoint. cmd/picolet/main.go is a thin wrapper that calls
// Execute; tests can invoke the same command tree in-process.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/schjan/picolet/pkg/agentcfg"
	mqttpkg "github.com/schjan/picolet/pkg/mqtt"
	"github.com/schjan/picolet/pkg/version"
)

// Execute builds the picolet CLI app and runs it against the given args.
// The provided ctx is wrapped with SIGINT/SIGTERM cancellation so every
// subcommand inherits signal-aware cancellation even when callers (main,
// tests) pass context.Background().
func Execute(ctx context.Context, args []string) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	app := newApp()
	if err := app.Run(ctx, args); err != nil {
		slog.Error("command failed", "error", err)
		return err
	}
	return nil
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "picolet",
		Usage:   "GitOps agent for Podman Quadlets",
		Version: version.Version,
		Flags:   rootFlags(),
		Before:  setupTextLogging,
		Commands: []*cli.Command{
			validateCmd(),
			resolveCmd(),
			runCmd(),
			healthcheckCmd(),
			triggerCmd(),
			dryRunCmd(),
			applyCmd(),
			downCmd(),
		},
	}
}

func rootFlags() []cli.Flag {
	return []cli.Flag{
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
	}
}

// setupTextLogging installs the default text-formatted slog handler. Used by
// short-lived subcommands; daemon-style subcommands replace it with JSON via
// jsonLogging in their own Before hook.
func setupTextLogging(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	level := slog.LevelInfo
	if cmd.Bool("verbose") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
	return ctx, nil
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

func validateCmd() *cli.Command {
	return &cli.Command{
		Name:  "validate",
		Usage: "validate all quadlet files and manifests for every host",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runValidate(ctx, cmd.Root().String("repo-dir"))
		},
	}
}

func resolveCmd() *cli.Command {
	return &cli.Command{
		Name:  "resolve",
		Usage: "render templates for a host and print desired state",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "host",
				Usage:    "hostname to resolve",
				Required: true,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runResolve(ctx, cmd.Root().String("repo-dir"), cmd.String("host"))
		},
	}
}

func runCmd() *cli.Command {
	return &cli.Command{
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
			return runAgent(ctx, cmd.String("config"))
		},
	}
}

func healthcheckCmd() *cli.Command {
	return &cli.Command{
		Name:  "healthcheck",
		Usage: "probe the running agent's health endpoint (for container healthcheck use)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Value:   "/etc/picolet/config.yml",
				Usage:   "agent config file (to read metrics port)",
				Sources: cli.EnvVars("PICOLET_CONFIG"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runHealthcheck(ctx, cmd.String("config"))
		},
	}
}

func triggerCmd() *cli.Command {
	return &cli.Command{
		Name:  "trigger",
		Usage: "trigger immediate reconciliation via webhook",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Value:   "/etc/picolet/config.yml",
				Usage:   "agent config file",
				Sources: cli.EnvVars("PICOLET_CONFIG"),
			},
			&cli.StringFlag{
				Name:  "url",
				Usage: "webhook URL override (default: http://localhost:<metrics_port>/webhook)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runTrigger(ctx, cmd.String("config"), cmd.String("url"))
		},
	}
}

func dryRunCmd() *cli.Command {
	return &cli.Command{
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
			&cli.StringFlag{
				Name:    "config",
				Usage:   "agent config file (enables secret resolution and config-aware state/paths)",
				Sources: cli.EnvVars("PICOLET_CONFIG"),
			},
		},
		Before: jsonLogging,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runDryRun(ctx, cmd.String("repo-dir"), cmd.String("host"), cmd.String("config"))
		},
	}
}

func applyCmd() *cli.Command {
	return &cli.Command{
		Name:  "apply",
		Usage: "one-shot reconciliation from local fleet files (must not run concurrently with 'picolet run')",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "host",
				Usage:    "hostname to apply",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "config",
				Value:   "/etc/picolet/config.yml",
				Usage:   "agent config file",
				Sources: cli.EnvVars("PICOLET_CONFIG"),
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
			return runApply(ctx, cmd.String("config"), cmd.String("repo-dir"), cmd.String("host"))
		},
	}
}

func downCmd() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "remove all managed resources (must not run concurrently with 'picolet run')",
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
			return runDown(ctx, cmd.String("config"))
		},
	}
}

// mqttConfigFrom converts an agent MQTT config to a pkg/mqtt Config, reading the password file if set.
func mqttConfigFrom(cfg *agentcfg.MQTTConfig) (mqttpkg.Config, error) {
	c := mqttpkg.Config{
		BrokerURL:   cfg.BrokerURL,
		Username:    cfg.Username,
		TopicPrefix: cfg.TopicPrefix,
	}
	if cfg.PasswordPath != "" {
		data, err := os.ReadFile(cfg.PasswordPath)
		if err != nil {
			return mqttpkg.Config{}, fmt.Errorf("reading MQTT password: %w", err)
		}
		c.Password = strings.TrimSpace(string(data))
	}
	return c, nil
}
