package cli

import (
	"context"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/schjan/picolet/pkg/bootstrap"
)

func bootstrapCmd() *cli.Command {
	return &cli.Command{
		Name:  "bootstrap",
		Usage: "provision picolet on a new host from a local fleet checkout",
		Flags: bootstrapRunFlags(),
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Args().First() == "create" {
				return setupTextLogging(ctx, cmd)
			}
			return jsonLogging(ctx, cmd)
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return bootstrap.Run(ctx, bootstrap.RunConfig{
				Hostname:     cmd.String("hostname"),
				RepoDir:      cmd.String("repo-dir"),
				Service:      cmd.String("service"),
				SystemdMode:  cmd.String("systemd"),
				Rootless:     cmd.Bool("rootless"),
				PodmanSocket: cmd.String("podman-socket"),
				SecretsDir:   cmd.String("secrets-dir"),
				HealthPath:   cmd.String("health-path"),
				MetricsPort:  cmd.Int("metrics-port"),
				Timeout:      cmd.Duration("timeout"),
				DataDir:      cmd.String("data-dir"),
				AllowRestart: cmd.Bool("allow-restart"),
			})
		},
		Commands: []*cli.Command{
			bootstrapCreateCmd(),
			bootstrapTeardownCmd(),
		},
	}
}

func bootstrapRunFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "hostname", Aliases: []string{"host"}, Usage: "hostname to bootstrap"},
		&cli.StringFlag{Name: "repo-dir", Usage: "local fleet repo path inside the bootstrap container"},
		&cli.StringFlag{Name: "service", Usage: "picolet service bundle to bootstrap"},
		&cli.StringFlag{Name: "systemd", Value: bootstrap.SystemdAuto, Usage: "systemd target: auto, user, or system"},
		&cli.BoolFlag{Name: "rootless", Usage: "use rootless/native path layout"},
		&cli.StringFlag{Name: "podman-socket", Value: "/run/podman/podman.sock", Usage: "Podman socket path inside the bootstrap container"},
		&cli.StringFlag{Name: "secrets-dir", Value: "/etc/picolet/secrets", Usage: "host-managed secrets directory inside the bootstrap container"},
		&cli.StringFlag{Name: "health-path", Value: "/health", Usage: "health endpoint path"},
		&cli.IntFlag{Name: "metrics-port", Usage: "health endpoint port override"},
		&cli.DurationFlag{Name: "timeout", Value: 0, Usage: "health wait timeout"},
		&cli.StringFlag{Name: "data-dir", Usage: "picolet data directory override"},
		&cli.BoolFlag{Name: "allow-restart", Usage: "allow restarting an already-active picolet when files changed"},
	}
}

func bootstrapCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "emit or run a host-specific bootstrap script",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "hostname", Aliases: []string{"host"}, Required: true, Usage: "hostname to bootstrap"},
			&cli.StringFlag{Name: "fleet-dir", Value: ".", Usage: "local fleet repo path"},
			&cli.StringFlag{Name: "service", Usage: "picolet service bundle to bootstrap"},
			&cli.StringFlag{Name: "target-path", Value: "/tmp/fleet", Usage: "target-side fleet repo path"},
			&cli.BoolFlag{Name: "script", Usage: "emit bare runnable script"},
			&cli.StringFlag{Name: "ssh", Usage: "rsync and run via ssh user@host"},
			&cli.BoolFlag{Name: "skip-git-checks", Usage: "skip dirty/ahead/behind safety checks"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return bootstrap.Create(ctx, bootstrap.CreateConfig{
				Hostname:      cmd.String("hostname"),
				FleetDir:      cmd.String("fleet-dir"),
				Service:       cmd.String("service"),
				TargetPath:    cmd.String("target-path"),
				Script:        cmd.Bool("script"),
				SSH:           cmd.String("ssh"),
				SkipGitChecks: cmd.Bool("skip-git-checks"),
				Stdout:        os.Stdout,
				Stderr:        os.Stderr,
			})
		},
	}
}

func bootstrapTeardownCmd() *cli.Command {
	return &cli.Command{
		Name:   "teardown",
		Usage:  "remove picolet-managed resources and bootstrap state",
		Flags:  bootstrapTeardownFlags(),
		Before: jsonLogging,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return bootstrap.Teardown(ctx, bootstrap.TeardownConfig{
				Hostname:     cmd.String("hostname"),
				Service:      cmd.String("service"),
				SystemdMode:  cmd.String("systemd"),
				Rootless:     cmd.Bool("rootless"),
				PodmanSocket: cmd.String("podman-socket"),
				DataDir:      cmd.String("data-dir"),
			})
		},
	}
}

func bootstrapTeardownFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "hostname", Aliases: []string{"host"}, Required: true, Usage: "hostname to tear down"},
		&cli.StringFlag{Name: "service", Usage: "picolet service bundle to tear down"},
		&cli.StringFlag{Name: "systemd", Value: bootstrap.SystemdAuto, Usage: "systemd target: auto, user, or system"},
		&cli.BoolFlag{Name: "rootless", Usage: "use rootless/native path layout"},
		&cli.StringFlag{Name: "podman-socket", Value: "/run/podman/podman.sock", Usage: "Podman socket path"},
		&cli.StringFlag{Name: "data-dir", Usage: "picolet data directory override"},
	}
}
