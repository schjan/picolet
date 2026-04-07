package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/github"
	"github.com/schjan/picolet/pkg/metrics"
	mqttpkg "github.com/schjan/picolet/pkg/mqtt"
	op "github.com/schjan/picolet/pkg/onepassword"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/rollback"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
	"github.com/schjan/picolet/pkg/version"
)

//nolint:funlen // declarative CLI registration; splitting reduces readability
func main() {
	app := &cli.Command{
		Name:    "picolet",
		Usage:   "GitOps agent for Podman Quadlets",
		Version: version.Version,
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
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runResolve(ctx, cmd.Root().String("repo-dir"), cmd.String("host"))
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
			},
			{
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
			},
			{
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
			},
			{
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

func runAgent(ctx context.Context, configPath string, dryRun bool) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.RepoURL == "" {
		return errors.New("repo_url is required for the run command")
	}

	metrics.Register()

	// Connect to systemd D-Bus
	systemd, err := applier.NewDBusSystemdManager(ctx, cfg.UseSystemdUser())
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	// Connect to Podman socket
	podman, err := applier.NewSocketPodmanClient(ctx, cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}

	opts := []agent.Option{
		agent.WithDryRun(dryRun),
		agent.WithSystemd(systemd),
		agent.WithPodman(podman),
		agent.WithFileWriter(applier.NewAtomicFileWriter()),
	}

	if cfg.MQTT != nil {
		mqttCfg, err := mqttConfigFrom(cfg.MQTT)
		if err != nil {
			return err
		}
		mqttClient, err := mqttpkg.NewClient(mqttCfg, cfg.Hostname)
		if err != nil {
			return err
		}
		opts = append(opts, agent.WithMQTT(mqttClient))
	} else {
		slog.Info("mqtt disabled")
	}

	if cfg.HasGitHubApp() {
		ghClient, appID, err := githubClientFromConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("creating GitHub client: %w", err)
		}

		opts = append(opts,
			agent.WithAuthProvider(ghClient),
			agent.WithDeploymentReporter(github.NewDeploymentReporter(ghClient, cfg.Hostname)),
		)
		slog.Info("github app auth enabled", "app_id", appID, "environment", cfg.Hostname)
	}

	a := agent.New(cfg, opts...)
	return a.Run(ctx)
}

func githubClientFromConfig(ctx context.Context, cfg *agentcfg.Config) (*github.Client, int64, error) {
	if cfg.HasGitHubAppRefs() {
		appID, installationID, privateKeyPEM, err := resolveGitHubAppFromOnePassword(ctx, cfg)
		if err != nil {
			return nil, 0, err
		}
		client, err := github.NewClientFromPEM(appID, installationID, privateKeyPEM, cfg.RepoURL)
		if err != nil {
			return nil, 0, err
		}
		return client, appID, nil
	}

	client, err := github.NewClient(cfg.GitHubAppID, cfg.GitHubInstallationID, cfg.GitHubPrivateKeyPath, cfg.RepoURL)
	if err != nil {
		return nil, 0, err
	}
	return client, cfg.GitHubAppID, nil
}

func resolveGitHubAppFromOnePassword(ctx context.Context, cfg *agentcfg.Config) (int64, int64, []byte, error) {
	appIDRaw, installationRaw, privateKeyRaw, err := resolveGitHubAppRefValues(ctx, cfg)
	if err != nil {
		return 0, 0, nil, err
	}
	return parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw)
}

func resolveGitHubAppRefValues(ctx context.Context, cfg *agentcfg.Config) (string, string, string, error) {
	reader, err := opReaderFromConfig(ctx, cfg)
	if err != nil {
		return "", "", "", err
	}
	if reader == nil || cfg.OnePassword == nil {
		return "", "", "", errors.New("onepassword must be configured to resolve github app refs")
	}

	refs := []string{
		cfg.OnePassword.GitHubAppIDRef,
		cfg.OnePassword.GitHubInstallationRef,
		cfg.OnePassword.GitHubPrivateKeyRef,
	}
	results, err := reader(ctx, refs)
	if err != nil {
		return "", "", "", fmt.Errorf("resolving github app refs from 1password: %w", err)
	}

	appIDRaw, err := resolvedRef(results, cfg.OnePassword.GitHubAppIDRef, "onepassword.github_app_id_ref")
	if err != nil {
		return "", "", "", err
	}
	installationRaw, err := resolvedRef(results, cfg.OnePassword.GitHubInstallationRef, "onepassword.github_installation_id_ref")
	if err != nil {
		return "", "", "", err
	}
	privateKeyRaw, err := resolvedRef(results, cfg.OnePassword.GitHubPrivateKeyRef, "onepassword.github_private_key_ref")
	if err != nil {
		return "", "", "", err
	}
	return appIDRaw, installationRaw, privateKeyRaw, nil
}

func parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw string) (int64, int64, []byte, error) {
	appID, err := parsePositiveInt64("onepassword.github_app_id_ref", appIDRaw)
	if err != nil {
		return 0, 0, nil, err
	}
	installationID, err := parsePositiveInt64("onepassword.github_installation_id_ref", installationRaw)
	if err != nil {
		return 0, 0, nil, err
	}

	privateKey := strings.TrimSpace(privateKeyRaw)
	if privateKey == "" {
		return 0, 0, nil, errors.New("onepassword.github_private_key_ref resolved to an empty value")
	}

	return appID, installationID, []byte(privateKey), nil
}

func resolvedRef(results map[string]string, ref, fieldName string) (string, error) {
	value, ok := results[ref]
	if !ok {
		return "", fmt.Errorf("resolving github app refs from 1password: missing %s (%q)", fieldName, ref)
	}
	return value, nil
}

func parsePositiveInt64(name, value string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid integer: %w", name, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return n, nil
}

// opReaderFromConfig creates an OpSecretReader from agent config, or nil if 1Password is not configured.
//
//nolint:nilnil // nil reader signals "1Password not configured"; matches OpSecretReader convention
func opReaderFromConfig(ctx context.Context, cfg *agentcfg.Config) (resolver.OpSecretReader, error) {
	if cfg.OnePassword == nil {
		return nil, nil
	}
	reader, err := op.NewReaderFromTokenFile(ctx, cfg.OnePassword.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("setting up 1password: %w", err)
	}
	return reader, nil
}

// dryRunResolveWithConfig resolves files using agent config (secrets, RepoSubDir, rootless-aware state).
func dryRunResolveWithConfig(ctx context.Context, repoDir, hostname, configPath string) ([]resolver.ResolvedFile, *state.Store, bool, error) {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return nil, nil, false, err
	}

	opReader, err := opReaderFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, false, err
	}

	files, err := agent.LoadAndResolve(ctx, effectiveRepoDir(repoDir, cfg.RepoSubDir), hostname, cfg.SecretsDir, cfg.Rootless, opReader)
	if err != nil {
		return nil, nil, false, err
	}

	store, err := stateStoreFromConfig(cfg)
	if err != nil {
		return nil, nil, false, err
	}

	return files, store, cfg.Rootless, nil
}

// dryRunResolveBasic resolves files without agent config (no secrets, default state path).
func dryRunResolveBasic(ctx context.Context, repoDir, hostname string) ([]resolver.ResolvedFile, *state.Store, error) {
	repoFS := os.DirFS(repoDir)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	if err != nil {
		return nil, nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(ctx, hostname)
	if err != nil {
		return nil, nil, err
	}

	return resolved.Files, state.NewStore(agent.DefaultStatePath), nil
}

func runDryRun(ctx context.Context, repoDir, hostname, configPath string) error {
	var (
		files    []resolver.ResolvedFile
		store    *state.Store
		rootless bool
		err      error
	)

	if configPath != "" {
		files, store, rootless, err = dryRunResolveWithConfig(ctx, repoDir, hostname, configPath)
	} else {
		files, store, err = dryRunResolveBasic(ctx, repoDir, hostname)
	}
	if err != nil {
		return err
	}

	if err := validator.ValidateFiles(files, rootless); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	st, err := store.Load()
	if err != nil {
		slog.Warn("could not load state, using empty state", "error", err)
		st = state.NewState()
	}

	changeset := reconciler.Diff(files, st)

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
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	if err != nil {
		return fmt.Errorf("creating resolver: %w", err)
	}
	return validator.ValidateAll(ctx, r, cfg)
}

func runResolve(ctx context.Context, repoDir, host string) error {
	repoFS := os.DirFS(repoDir)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	if err != nil {
		return fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(ctx, host)
	if err != nil {
		return err
	}
	for _, f := range resolved.Files {
		fmt.Printf("=== %s ===\n%s\n", f.DestPath, f.Content)
	}
	return nil
}

//nolint:cyclop // MQTT path + webhook path with optional secret; splitting reduces readability
func runTrigger(_ context.Context, configPath, urlOverride string) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}

	// Prefer MQTT when configured and no explicit webhook URL override.
	if cfg.MQTT != nil && urlOverride == "" {
		mqttCfg, err := mqttConfigFrom(cfg.MQTT)
		if err != nil {
			return err
		}
		triggerCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return mqttpkg.Trigger(triggerCtx, mqttCfg) //nolint:contextcheck // intentional detached context — trigger must not inherit signal cancellation
	}

	webhookURL := urlOverride
	if webhookURL == "" {
		webhookURL = fmt.Sprintf("http://localhost:%d/webhook", cfg.MetricsPort)
	}

	var body []byte

	triggerCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(triggerCtx, http.MethodPost, webhookURL, bytes.NewReader(body)) //nolint:contextcheck // intentional detached context — trigger must not inherit signal cancellation
	if err != nil {
		return fmt.Errorf("building trigger request: %w", err)
	}

	if cfg.WebhookSecretPath != "" {
		secretData, err := os.ReadFile(cfg.WebhookSecretPath)
		if err != nil {
			return fmt.Errorf("reading webhook secret: %w", err)
		}
		secret := strings.TrimSpace(string(secretData))
		sig := agent.ComputeSignature(body, secret)
		req.Header.Set("X-Hub-Signature-256", sig)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("trigger request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted:
		slog.Info("reconciliation triggered")
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("forbidden (status 403) — webhook secret may be required or incorrect")
	default:
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

// effectiveRepoDir applies an optional subdirectory to the repo root.
func effectiveRepoDir(repoDir, subDir string) string {
	if subDir != "" {
		return filepath.Join(repoDir, subDir)
	}
	return repoDir
}

// stateStoreFromConfig returns a state store using the config's DataDir or a rootless-aware default.
func stateStoreFromConfig(cfg *agentcfg.Config) (*state.Store, error) {
	if cfg.DataDir != "" {
		return state.NewStore(filepath.Join(cfg.DataDir, "state.json")), nil
	}
	_, _, dataDir, err := resolver.ResolveDirs(cfg.Rootless)
	if err != nil {
		return nil, fmt.Errorf("resolving data directory: %w", err)
	}
	return state.NewStore(filepath.Join(dataDir, "state.json")), nil
}

// applyWithRollback snapshots the current state, applies changes, and rolls back on fatal error.
func applyWithRollback(
	ctx context.Context,
	changeset *reconciler.Changeset,
	systemd applier.SystemdManager,
	podman applier.PodmanClient,
) (*applier.ApplyResult, error) {
	writer := applier.NewAtomicFileWriter()
	rollbackMgr := rollback.New(writer, systemd)
	snap, err := rollbackMgr.Create(changeset, os.ReadFile)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	app := applier.New(systemd, podman, writer, false)
	result, err := app.Apply(ctx, changeset)
	if err != nil {
		slog.Error("apply failed, rolling back", "error", err)
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if rbErr := rollbackMgr.Restore(rollbackCtx, snap); rbErr != nil {
			slog.Error("rollback failed", "error", rbErr)
		}
		return nil, fmt.Errorf("apply failed: %w", err)
	}
	for _, e := range result.Errors {
		slog.Warn("non-fatal apply error", "error", e)
	}

	return result, nil
}

//nolint:cyclop,funlen // sequential apply steps; splitting reduces readability
func runApply(ctx context.Context, configPath, repoDir, hostname string) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}

	opReader, err := opReaderFromConfig(ctx, cfg)
	if err != nil {
		return err
	}

	files, err := agent.LoadAndResolve(ctx, effectiveRepoDir(repoDir, cfg.RepoSubDir), hostname, cfg.SecretsDir, cfg.Rootless, opReader)
	if err != nil {
		return err
	}

	store, err := stateStoreFromConfig(cfg)
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	changeset := reconciler.Diff(files, st)
	if !changeset.HasChanges() {
		slog.Info("no changes to apply")
		return nil
	}

	if err := validator.ValidateFiles(files, cfg.Rootless); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	slog.Info("applying changes",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	systemd, err := applier.NewDBusSystemdManager(ctx, cfg.UseSystemdUser())
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}

	result, err := applyWithRollback(ctx, changeset, systemd, podman)
	if err != nil {
		return err
	}

	slog.Info("apply complete", "applied", result.Applied, "restarted", result.RestartedUnits)

	st.MarkApplied(fmt.Sprintf("local-%d", time.Now().Unix()))
	agent.UpdateState(st, changeset)
	return store.Save(st)
}

func runDown(ctx context.Context, configPath string) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}

	store, err := stateStoreFromConfig(cfg)
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	if len(st.ManagedFiles) == 0 {
		slog.Info("nothing to tear down")
		return nil
	}

	systemd, err := applier.NewDBusSystemdManager(ctx, cfg.UseSystemdUser())
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}

	changeset := reconciler.Diff(nil, st)
	app := applier.New(systemd, podman, applier.NewAtomicFileWriter(), false)
	result, err := app.Apply(ctx, changeset)
	if err != nil {
		return fmt.Errorf("teardown failed: %w", err)
	}
	for _, e := range result.Errors {
		slog.Warn("non-fatal teardown error", "error", e)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("teardown incomplete: %d resource(s) not removed", len(result.Errors))
	}

	slog.Info("down complete", "deleted", changeset.Summary[reconciler.ActionDelete])
	return store.Save(state.NewState())
}

func runHealthcheck(_ context.Context, configPath string) error {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return err
	}
	// 5s timeout: must complete well within HealthTimeout in the Quadlet.
	// Uses Background() instead of the parent signal context so SIGTERM during
	// container shutdown doesn't spuriously cancel the probe.
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://localhost:%d/health", cfg.MetricsPort)
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil) //nolint:contextcheck // intentional detached context — health probe must not inherit signal cancellation
	if err != nil {
		return fmt.Errorf("building health check request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: status %d", resp.StatusCode)
	}
	return nil
}
