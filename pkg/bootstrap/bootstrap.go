package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

const (
	SystemdAuto   = "auto"
	SystemdUser   = "user"
	SystemdSystem = "system"

	defaultPodmanSocket = "/run/podman/podman.sock"
	defaultSecretsDir   = "/etc/picolet/secrets" //nolint:gosec // default path, not a credential value.
	defaultHealthPath   = "/health"
	defaultTimeout      = 90 * time.Second
)

type RunConfig struct {
	Hostname     string
	RepoDir      string
	Service      string
	SystemdMode  string
	Rootless     bool
	PodmanSocket string
	SecretsDir   string
	HealthPath   string
	MetricsPort  int
	Timeout      time.Duration
	DataDir      string
	AllowRestart bool
}

type TeardownConfig struct {
	Hostname     string
	Service      string
	SystemdMode  string
	Rootless     bool
	PodmanSocket string
	DataDir      string
}

func Run(ctx context.Context, cfg RunConfig) error { //nolint:cyclop,funlen // orchestration mirrors the documented bootstrap sequence.
	cfg = normalizeRunConfig(cfg)
	useSystemdUser, err := resolveSystemdMode(cfg.SystemdMode)
	if err != nil {
		return err
	}
	service := cfg.Service
	if service == "" {
		service = defaultService(useSystemdUser)
	}
	dataDir, err := dataDir(cfg.Rootless, cfg.DataDir)
	if err != nil {
		return err
	}

	resolved, err := resolveBootstrapHost(ctx, resolveConfig{
		RepoDir:    cfg.RepoDir,
		Hostname:   cfg.Hostname,
		Service:    service,
		Rootless:   cfg.Rootless,
		DataDir:    dataDir,
		SecretsDir: cfg.SecretsDir,
		FileMode:   fileReaderStrict,
	})
	if err != nil {
		return err
	}
	if err := validator.ValidateFiles(resolved.Files, cfg.Rootless); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	unitName, err := picoletUnitName(resolved.Files)
	if err != nil {
		return err
	}
	metricsPort := cfg.MetricsPort
	if metricsPort == 0 {
		metricsPort, err = metricsPortFromResolved(resolved.Files)
		if err != nil {
			return err
		}
	}

	systemd, err := applier.NewDBusSystemdManager(ctx, useSystemdUser)
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	unitState, err := systemd.UnitState(ctx, unitName)
	if err != nil {
		return fmt.Errorf("checking %s state: %w", unitName, err)
	}
	active := unitState == "active" || unitState == "activating" || unitState == "reloading"

	store := state.NewStore(filepath.Join(dataDir, "state.json"))
	if active {
		done, err := handleActiveBootstrap(ctx, systemd, store, resolved.Files, unitName, metricsPort, cfg)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		active = false
	}

	releaseLock, err := agent.AcquireLock(filepath.Join(dataDir, "reconciliation.lock"))
	if err != nil {
		return err
	}
	defer func() {
		if err := releaseLock(); err != nil {
			slog.Warn("releasing bootstrap lock failed", "error", err)
		}
	}()

	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	changeset := diffBootstrapScope(resolved.Files, st, unitName)

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}
	app := applier.New(systemd, podman, applier.NewAtomicFileWriter(), false, nil)
	result, err := app.ApplyWithoutRestarts(ctx, changeset)
	if err != nil {
		return fmt.Errorf("bootstrap apply failed: %w", err)
	}
	for _, e := range result.Errors {
		slog.Warn("non-fatal bootstrap apply error", "error", e)
	}

	st, err = store.Load()
	if err != nil {
		return fmt.Errorf("reloading state: %w", err)
	}
	reconciler.MergeChangeset(st, changeset)
	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	if err := systemd.EnableUnit(ctx, unitName); err != nil {
		return err
	}
	if active {
		if err := systemd.RestartUnit(ctx, unitName); err != nil {
			return err
		}
	} else {
		if err := systemd.StartUnit(ctx, unitName); err != nil {
			return err
		}
	}
	return WaitForHealth(ctx, metricsPort, cfg.HealthPath, cfg.Timeout)
}

func handleActiveBootstrap(ctx context.Context, systemd applier.SystemdManager, store *state.Store, files []resolver.ResolvedFile, unitName string, metricsPort int, cfg RunConfig) (bool, error) {
	st, err := store.Load()
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	guardChangeset := diffBootstrapScope(files, st, unitName)
	if guardChangeset.HasChanges() && !cfg.AllowRestart {
		return false, fmt.Errorf("%s is already active and bootstrap would change its managed files; stop it first or pass --allow-restart", unitName)
	}
	if !guardChangeset.HasChanges() {
		if err := systemd.EnableUnit(ctx, unitName); err != nil {
			return false, err
		}
		return true, WaitForHealth(ctx, metricsPort, cfg.HealthPath, cfg.Timeout)
	}
	slog.Info("stopping active picolet before bootstrap re-apply", "unit", unitName)
	if err := systemd.StopUnit(ctx, unitName); err != nil {
		return false, err
	}
	return false, nil
}

func Teardown(ctx context.Context, cfg TeardownConfig) error { //nolint:cyclop,funlen // explicit teardown sequence.
	cfg = normalizeTeardownConfig(cfg)
	useSystemdUser, err := resolveSystemdMode(cfg.SystemdMode)
	if err != nil {
		return err
	}
	service := cfg.Service
	if service == "" {
		service = defaultService(useSystemdUser)
	}
	dataDir, err := dataDir(cfg.Rootless, cfg.DataDir)
	if err != nil {
		return err
	}
	unitName := service + ".service"

	systemd, err := applier.NewDBusSystemdManager(ctx, useSystemdUser)
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()
	if err := systemd.DisableUnit(ctx, unitName); err != nil {
		slog.Warn("disabling unit", "unit", unitName, "error", err)
	}
	if err := systemd.StopUnit(ctx, unitName); err != nil {
		slog.Warn("stopping unit", "unit", unitName, "error", err)
	}

	releaseLock, err := agent.AcquireLock(filepath.Join(dataDir, "reconciliation.lock"))
	if err != nil {
		return err
	}
	defer func() {
		if err := releaseLock(); err != nil {
			slog.Warn("releasing bootstrap lock failed", "error", err)
		}
	}()

	store := state.NewStore(filepath.Join(dataDir, "state.json"))
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	changeset := reconciler.Diff(nil, st)
	if !changeset.HasChanges() {
		return removeBootstrapState(dataDir)
	}

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}
	app := applier.New(systemd, podman, applier.NewAtomicFileWriter(), false, nil)
	result, err := app.ApplyWithoutRestarts(ctx, changeset)
	if err != nil {
		return fmt.Errorf("bootstrap teardown failed: %w", err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("bootstrap teardown incomplete: %w", errors.Join(result.Errors...))
	}
	return removeBootstrapState(dataDir)
}

func normalizeRunConfig(cfg RunConfig) RunConfig {
	if cfg.SystemdMode == "" {
		cfg.SystemdMode = SystemdAuto
	}
	if cfg.PodmanSocket == "" {
		cfg.PodmanSocket = defaultPodmanSocket
	}
	if cfg.SecretsDir == "" {
		cfg.SecretsDir = defaultSecretsDir
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = defaultHealthPath
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	return cfg
}

func normalizeTeardownConfig(cfg TeardownConfig) TeardownConfig {
	if cfg.SystemdMode == "" {
		cfg.SystemdMode = SystemdAuto
	}
	if cfg.PodmanSocket == "" {
		cfg.PodmanSocket = defaultPodmanSocket
	}
	return cfg
}

func resolveSystemdMode(mode string) (bool, error) {
	switch mode {
	case "", SystemdAuto:
		return agentcfg.DetectSystemdUser(), nil
	case SystemdUser:
		return true, nil
	case SystemdSystem:
		return false, nil
	default:
		return false, fmt.Errorf("invalid --systemd %q (want auto, user, or system)", mode)
	}
}

func defaultService(useSystemdUser bool) string {
	if useSystemdUser {
		return "picolet"
	}
	return "picolet-system"
}

func dataDir(rootless bool, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	_, _, dir, err := resolver.ResolveDirs(rootless)
	return dir, err
}

func picoletUnitName(files []resolver.ResolvedFile) (string, error) {
	for _, file := range files {
		if file.ServiceName != "" && strings.Contains(file.ServiceName, "picolet") {
			return file.ServiceName, nil
		}
	}
	for _, file := range files {
		if file.ServiceName != "" {
			return file.ServiceName, nil
		}
	}
	return "", fmt.Errorf("resolved picolet service has no systemd unit")
}

func metricsPortFromResolved(files []resolver.ResolvedFile) (int, error) {
	file, err := configSecret(files)
	if err != nil {
		return 0, err
	}
	cfg, err := agentcfg.Parse([]byte(file.Content))
	if err != nil {
		return 0, fmt.Errorf("parsing rendered picolet config: %w", err)
	}
	return cfg.MetricsPort, nil
}

func configSecret(files []resolver.ResolvedFile) (*resolver.ResolvedFile, error) {
	for i := range files {
		if files[i].Category == config.CategorySecret && strings.Contains(files[i].DestPath, "picolet") && strings.Contains(files[i].DestPath, "config") {
			return &files[i], nil
		}
	}
	return nil, fmt.Errorf("resolved picolet service did not produce a picolet config secret")
}

func diffBootstrapScope(files []resolver.ResolvedFile, st *state.State, unitName string) *reconciler.Changeset {
	return reconciler.Diff(files, bootstrapScopedState(files, st, unitName))
}

func bootstrapScopedState(files []resolver.ResolvedFile, st *state.State, unitName string) *state.State {
	scoped := state.NewState()
	desired := make(map[string]struct{}, len(files))
	for _, file := range files {
		desired[file.DestPath] = struct{}{}
	}
	for destPath, mf := range st.ManagedFiles {
		if _, ok := desired[destPath]; ok || st.ServiceNames[destPath] == unitName {
			scoped.ManagedFiles[destPath] = mf
			if serviceName := st.ServiceNames[destPath]; serviceName != "" {
				scoped.ServiceNames[destPath] = serviceName
			}
		}
	}
	return scoped
}

func removeBootstrapState(dataDir string) error {
	var errs []error
	if err := os.Remove(filepath.Join(dataDir, "state.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "repo")); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
