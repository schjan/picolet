package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

	// agentConfigTarget is the in-container path the agent reads its config
	// from; the bundle's container quadlet mounts the config secret there.
	agentConfigTarget = "/etc/picolet/config.yml"
)

// Target identifies the picolet service bundle and systemd instance a
// bootstrap command operates on.
type Target struct {
	Service      string
	SystemdMode  string
	Rootless     bool
	PodmanSocket string
	DataDir      string
}

type RunConfig struct {
	Target

	Hostname     string
	RepoDir      string
	SecretsDir   string
	HealthPath   string
	MetricsPort  int
	Timeout      time.Duration
	AllowRestart bool
}

type TeardownConfig struct {
	Target

	Hostname string
}

// resolvedTarget is a Target with mode detection and defaulting applied.
type resolvedTarget struct {
	useSystemdUser bool
	service        string
	unitName       string
	dataDir        string
}

func (t Target) resolve() (resolvedTarget, error) {
	useSystemdUser, err := resolveSystemdMode(t.SystemdMode)
	if err != nil {
		return resolvedTarget{}, err
	}
	service := t.Service
	if service == "" {
		service = defaultService(useSystemdUser)
	}
	dir, err := dataDir(t.Rootless, t.DataDir)
	if err != nil {
		return resolvedTarget{}, err
	}
	return resolvedTarget{
		useSystemdUser: useSystemdUser,
		service:        service,
		unitName:       service + ".service",
		dataDir:        dir,
	}, nil
}

func (t Target) podmanSocket() string {
	if t.PodmanSocket == "" {
		return defaultPodmanSocket
	}
	return t.PodmanSocket
}

func Run(ctx context.Context, cfg RunConfig) error { //nolint:cyclop,funlen // orchestration mirrors the documented bootstrap sequence.
	cfg = normalizeRunConfig(cfg)
	tgt, err := cfg.resolve()
	if err != nil {
		return err
	}

	resolved, err := resolveBootstrapHost(ctx, resolveConfig{
		RepoDir:    cfg.RepoDir,
		Hostname:   cfg.Hostname,
		Service:    tgt.service,
		Rootless:   cfg.Rootless,
		DataDir:    tgt.dataDir,
		SecretsDir: cfg.SecretsDir,
		FileMode:   fileReaderStrict,
	})
	if err != nil {
		return err
	}
	if err := validator.ValidateFiles(resolved.Files, cfg.Rootless); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if err := verifyUnitResolved(resolved.Files, tgt); err != nil {
		return err
	}
	metricsPort := cfg.MetricsPort
	if metricsPort == 0 {
		metricsPort, err = metricsPortFromResolved(resolved.Files, tgt.unitName)
		if err != nil {
			return err
		}
	}

	systemd, err := applier.NewDBusSystemdManager(ctx, tgt.useSystemdUser)
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()

	status, err := systemd.GetUnitStatus(ctx, tgt.unitName)
	if err != nil {
		return fmt.Errorf("checking %s state: %w", tgt.unitName, err)
	}
	store := state.NewStore(filepath.Join(tgt.dataDir, "state.json"))
	if slices.Contains([]string{"active", "activating", "reloading"}, status.ActiveState) {
		done, err := handleActiveBootstrap(ctx, systemd, store, resolved.Files, tgt.unitName, metricsPort, cfg)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	releaseLock, err := agent.AcquireLock(filepath.Join(tgt.dataDir, "reconciliation.lock"))
	if err != nil {
		return err
	}
	defer func() {
		if releaseLock == nil {
			return
		}
		if err := releaseLock(); err != nil {
			slog.Warn("releasing bootstrap lock failed", "error", err)
		}
	}()

	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	changeset := diffBootstrapScope(resolved.Files, st, tgt.unitName)

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.podmanSocket())
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}
	// WithSelfUnits(): bootstrap runs outside any managed unit, so the
	// applier's self-protection must not skip stops of agent-named units.
	app := applier.New(systemd, podman, applier.NewAtomicFileWriter(), false, nil, applier.WithSelfUnits())
	result, err := app.ApplyWithoutRestarts(ctx, changeset)
	if err != nil {
		return fmt.Errorf("bootstrap apply failed: %w", err)
	}
	for _, e := range result.Errors {
		slog.Warn("non-fatal bootstrap apply error", "error", e)
	}

	reconciler.MergeChangeset(st, changeset)
	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	// Release the lock BEFORE starting the agent: its first act is acquiring
	// this same lock (shared via the data-dir bind mount), and it exits before
	// serving /health when the lock is held — holding it through WaitForHealth
	// would deadlock the bootstrap against the agent it just started.
	if err := releaseLock(); err != nil {
		slog.Warn("releasing bootstrap lock failed", "error", err)
	}
	releaseLock = nil

	// No explicit enable: the unit is quadlet-generated (systemd refuses to
	// enable generated units); quadlet itself realizes [Install] WantedBy=.
	if err := systemd.StartUnit(ctx, tgt.unitName); err != nil {
		return err
	}
	return WaitForHealth(ctx, metricsPort, cfg.HealthPath, cfg.Timeout)
}

// handleActiveBootstrap guards against re-bootstrapping under a live agent.
// Returns done=true when the active agent already matches the desired state;
// otherwise (with AllowRestart) the unit is stopped and bootstrap proceeds.
func handleActiveBootstrap(ctx context.Context, systemd applier.SystemdManager, store *state.Store, files []resolver.ResolvedFile, unitName string, metricsPort int, cfg RunConfig) (bool, error) {
	st, err := store.Load()
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	guardChangeset := diffBootstrapScope(files, st, unitName)
	if !guardChangeset.HasChanges() {
		return true, WaitForHealth(ctx, metricsPort, cfg.HealthPath, cfg.Timeout)
	}
	if !cfg.AllowRestart {
		return false, fmt.Errorf("%s is already active and bootstrap would change its managed files; stop it first or pass --allow-restart", unitName)
	}
	slog.Info("stopping active picolet before bootstrap re-apply", "unit", unitName)
	if err := systemd.StopUnit(ctx, unitName); err != nil {
		return false, err
	}
	return false, nil
}

func Teardown(ctx context.Context, cfg TeardownConfig) error { //nolint:cyclop // explicit teardown sequence.
	tgt, err := cfg.resolve()
	if err != nil {
		return err
	}
	if err := verifyTeardownHost(cfg.Hostname); err != nil {
		return err
	}

	systemd, err := applier.NewDBusSystemdManager(ctx, tgt.useSystemdUser)
	if err != nil {
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer systemd.Close()
	// No explicit disable: the unit is quadlet-generated and cannot be
	// disabled via systemd; removing the .container file removes the unit.
	if err := systemd.StopUnit(ctx, tgt.unitName); err != nil {
		slog.Warn("stopping unit", "unit", tgt.unitName, "error", err)
	}

	releaseLock, err := agent.AcquireLock(filepath.Join(tgt.dataDir, "reconciliation.lock"))
	if err != nil {
		return err
	}
	defer func() {
		if err := releaseLock(); err != nil {
			slog.Warn("releasing bootstrap lock failed", "error", err)
		}
	}()

	store := state.NewStore(filepath.Join(tgt.dataDir, "state.json"))
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	changeset := reconciler.Diff(nil, st)
	if !changeset.HasChanges() {
		return removeBootstrapState(tgt.dataDir)
	}

	podman, err := applier.NewSocketPodmanClient(ctx, cfg.podmanSocket())
	if err != nil {
		return fmt.Errorf("connecting to podman: %w", err)
	}
	app := applier.New(systemd, podman, applier.NewAtomicFileWriter(), false, nil, applier.WithSelfUnits())
	result, err := app.ApplyWithoutRestarts(ctx, changeset)
	if err != nil {
		return fmt.Errorf("bootstrap teardown failed: %w", err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("bootstrap teardown incomplete: %w", errors.Join(result.Errors...))
	}
	return removeBootstrapState(tgt.dataDir)
}

// verifyTeardownHost guards against tearing down the wrong host: when a local
// agent config is readable, its hostname must match the requested one. Inside
// the ad-hoc bootstrap container no config file exists (the agent's config is
// a podman secret), so the check degrades to a warning there.
func verifyTeardownHost(hostname string) error {
	if hostname == "" {
		return nil
	}
	path := os.Getenv("PICOLET_CONFIG")
	if path == "" {
		path = agentConfigTarget
	}
	cfg, err := agentcfg.Load(path)
	if err != nil {
		slog.Warn("cannot verify --hostname against local agent config; proceeding", "path", path, "error", err)
		return nil
	}
	if cfg.Hostname != hostname {
		return fmt.Errorf("refusing teardown: --hostname %s does not match local agent config hostname %s", hostname, cfg.Hostname)
	}
	return nil
}

func normalizeRunConfig(cfg RunConfig) RunConfig {
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

// verifyUnitResolved checks that the resolved bundle contains the quadlet
// whose generated unit bootstrap will start (<service>.service by convention).
func verifyUnitResolved(files []resolver.ResolvedFile, tgt resolvedTarget) error {
	var units []string
	for _, file := range files {
		if file.ServiceName == tgt.unitName {
			return nil
		}
		if file.ServiceName != "" {
			units = append(units, file.ServiceName)
		}
	}
	return fmt.Errorf("service %s did not resolve a quadlet unit named %s (resolved units: %s)",
		tgt.service, tgt.unitName, strings.Join(units, ", "))
}

func metricsPortFromResolved(files []resolver.ResolvedFile, unitName string) (int, error) {
	file, err := configSecret(files, unitName)
	if err != nil {
		return 0, err
	}
	cfg, err := agentcfg.Parse([]byte(file.Content))
	if err != nil {
		return 0, fmt.Errorf("parsing rendered picolet config: %w", err)
	}
	return cfg.MetricsPort, nil
}

// configSecret returns the resolved secret the bundle's container quadlet
// mounts at the agent config path (Secret=<name>,target=/etc/picolet/config.yml).
func configSecret(files []resolver.ResolvedFile, unitName string) (*resolver.ResolvedFile, error) {
	name := ""
	for _, file := range files {
		if file.ServiceName != unitName || file.ParsedUnit == nil {
			continue
		}
		for _, secret := range file.ParsedUnit.LookupAll("Container", "Secret") {
			parts := strings.Split(secret, ",")
			if slices.Contains(parts[1:], "target="+agentConfigTarget) {
				name = parts[0]
			}
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%s declares no Secret=...,target=%s mount for the agent config", unitName, agentConfigTarget)
	}
	for i := range files {
		if files[i].Category == config.CategorySecret && files[i].DestPath == "secret:"+name {
			return &files[i], nil
		}
	}
	return nil, fmt.Errorf("config secret %s referenced by %s was not resolved by the bundle", name, unitName)
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
