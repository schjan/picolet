package cli

import (
	"context"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/schjan/picolet/pkg/bootstrap"
)

func bootstrapCmd() *cli.Command {
	return &cli.Command{
		Name:   "bootstrap",
		Usage:  "provision picolet on a new host from a local fleet checkout",
		Flags:  bootstrapRunFlags(),
		Before: jsonLogging,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return bootstrap.Run(ctx, bootstrap.RunConfig{
				Target:       bootstrapTarget(cmd),
				Hostname:     cmd.String("hostname"),
				RepoDir:      cmd.String("repo-dir"),
				SecretsDir:   cmd.String("secrets-dir"),
				HealthPath:   cmd.String("health-path"),
				MetricsPort:  cmd.Int("metrics-port"),
				Timeout:      cmd.Duration("timeout"),
				AllowRestart: cmd.Bool("allow-restart"),
			})
		},
		Commands: []*cli.Command{
			bootstrapCreateCmd(),
			bootstrapTeardownCmd(),
		},
	}
}

// bootstrapTarget collects the flags shared by bootstrap and teardown.
func bootstrapTarget(cmd *cli.Command) bootstrap.Target {
	return bootstrap.Target{
		Service:      cmd.String("service"),
		SystemdMode:  cmd.String("systemd"),
		Rootless:     cmd.Bool("rootless"),
		PodmanSocket: cmd.String("podman-socket"),
		DataDir:      cmd.String("data-dir"),
	}
}

func bootstrapTargetFlags(socketUsage string) []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "service", Usage: "picolet service bundle"},
		&cli.StringFlag{Name: "systemd", Value: bootstrap.SystemdAuto, Usage: "systemd target: auto, user, or system"},
		&cli.BoolFlag{Name: "rootless", Usage: "use rootless/native path layout"},
		&cli.StringFlag{Name: "podman-socket", Value: "/run/podman/podman.sock", Usage: socketUsage},
		&cli.StringFlag{Name: "data-dir", Usage: "picolet data directory override"},
	}
}

func bootstrapRunFlags() []cli.Flag {
	return append(bootstrapTargetFlags("Podman socket path inside the bootstrap container"),
		&cli.StringFlag{Name: "hostname", Aliases: []string{"host"}, Usage: "hostname to bootstrap"},
		&cli.StringFlag{Name: "repo-dir", Usage: "local fleet repo path inside the bootstrap container"},
		&cli.StringFlag{Name: "secrets-dir", Value: "/etc/picolet/secrets", Usage: "host-managed secrets directory inside the bootstrap container"},
		&cli.StringFlag{Name: "health-path", Value: "/health", Usage: "health endpoint path"},
		&cli.IntFlag{Name: "metrics-port", Usage: "health endpoint port override"},
		&cli.DurationFlag{Name: "timeout", Value: 0, Usage: "health wait timeout"},
		&cli.BoolFlag{Name: "allow-restart", Usage: "allow restarting an already-active picolet when files changed"},
	)
}

func bootstrapCreateCmd() *cli.Command {
	return &cli.Command{
		Name:   "create",
		Usage:  "emit or run a host-specific bootstrap script",
		Before: setupTextLogging,
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
		Name:  "teardown",
		Usage: "remove ALL picolet-managed resources on this host and the bootstrap state",
		Flags: append(bootstrapTargetFlags("Podman socket path"),
			&cli.StringFlag{Name: "hostname", Aliases: []string{"host"}, Usage: "hostname to tear down (verified against the local agent config when readable)"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return bootstrap.Teardown(ctx, bootstrap.TeardownConfig{
				Target:   bootstrapTarget(cmd),
				Hostname: cmd.String("hostname"),
			})
		},
	}
}
