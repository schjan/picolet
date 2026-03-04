package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/rollback"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

const (
	defaultRepoPath  = "/var/lib/picolet/repo"
	defaultStatePath = "/var/lib/picolet/state.json"
	lockPath         = "/var/lib/picolet/reconciliation.lock"
)

// Agent is the main reconciliation loop.
type Agent struct {
	cfg     *agentcfg.Config
	dryRun  bool
	systemd applier.SystemdManager
	podman  applier.PodmanClient
	writer  applier.FileWriter

	// Overridable for testing
	repoPath  string
	statePath string
	lockPath  string
}

// Option configures the Agent.
type Option func(*Agent)

// WithDryRun enables dry-run mode.
func WithDryRun(b bool) Option {
	return func(a *Agent) { a.dryRun = b }
}

// WithSystemd overrides the SystemdManager (for testing).
func WithSystemd(s applier.SystemdManager) Option {
	return func(a *Agent) { a.systemd = s }
}

// WithPodman overrides the PodmanClient (for testing).
func WithPodman(p applier.PodmanClient) Option {
	return func(a *Agent) { a.podman = p }
}

// WithFileWriter overrides the FileWriter (for testing).
func WithFileWriter(w applier.FileWriter) Option {
	return func(a *Agent) { a.writer = w }
}

// WithRepoPath overrides the local repo clone path.
func WithRepoPath(path string) Option {
	return func(a *Agent) { a.repoPath = path }
}

// WithStatePath overrides the state file path.
func WithStatePath(path string) Option {
	return func(a *Agent) { a.statePath = path }
}

// WithLockPath overrides the reconciliation lock file path.
func WithLockPath(path string) Option {
	return func(a *Agent) { a.lockPath = path }
}

// New creates a new Agent.
func New(cfg *agentcfg.Config, opts ...Option) *Agent {
	a := &Agent{
		cfg:       cfg,
		repoPath:  defaultRepoPath,
		statePath: defaultStatePath,
		lockPath:  lockPath,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run starts the reconciliation loop. Blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// Start metrics server
	go a.serveMetrics(ctx)

	// Check for stale lock (unclean shutdown)
	if _, err := os.Stat(a.lockPath); err == nil {
		slog.Warn("stale reconciliation lock found, will force full reconciliation")
		if err := os.Remove(a.lockPath); err != nil {
			slog.Warn("removing stale lock failed", "path", a.lockPath, "error", err)
		}
	}

	// Initialize git poller
	poller := gitpoll.New(a.cfg.RepoURL, a.cfg.RepoBranch, a.repoPath, a.cfg.GitTokenPath)
	if err := poller.Init(ctx); err != nil {
		return fmt.Errorf("initializing git poller: %w", err)
	}

	store := state.NewStore(a.statePath)
	healthChecker := health.New(a.systemd)

	slog.Info("agent started",
		"hostname", a.cfg.Hostname,
		"poll_interval", a.cfg.PollInterval,
		"dry_run", a.dryRun,
	)

	// Run first tick immediately
	if err := a.tick(ctx, poller, store, healthChecker); err != nil {
		slog.Error("initial reconciliation failed", "error", err)
	}

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("agent shutting down")
			return nil
		case <-ticker.C:
			if err := a.tick(ctx, poller, store, healthChecker); err != nil {
				slog.Error("reconciliation tick failed", "error", err)
			}
		}
	}
}

//nolint:cyclop,funlen // multiple early-returns are clearer than restructuring
func (a *Agent) tick(ctx context.Context, poller *gitpoll.Poller, store *state.Store, healthChecker *health.Checker) error {
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// 1. Health enforcement (always)
	hr, err := healthChecker.Enforce(ctx, st)
	if err != nil {
		slog.Error("health enforcement error", "error", err)
	} else {
		for _, u := range hr.Healthy {
			metrics.HealthCheckTotal.WithLabelValues(u, "healthy").Inc()
		}
		for _, u := range hr.Unhealthy {
			metrics.HealthCheckTotal.WithLabelValues(u, "unhealthy").Inc()
			metrics.HealthEnforcementTotal.WithLabelValues(u, "restart").Inc()
			metrics.DriftDetectedTotal.Inc()
		}
	}

	metrics.SelfUpdatePending.Set(0)

	// 2. Git poll
	pollResult, err := poller.Poll(ctx, st.AppliedSHA)
	if err != nil {
		return fmt.Errorf("polling git: %w", err)
	}

	if !pollResult.Changed {
		slog.Debug("no git changes", "sha", pollResult.HeadSHA)
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		return nil
	}

	// Skip only after >= 3 consecutive failures on the same SHA
	const maxRetries = 3
	if pollResult.HeadSHA == st.FailedSHA && st.FailedCount >= maxRetries {
		slog.Warn("skipping permanently failed SHA",
			"sha", pollResult.HeadSHA,
			"failures", st.FailedCount,
		)
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		return nil
	}

	slog.Info("new git commit detected", "sha", pollResult.HeadSHA, "prev", st.AppliedSHA)

	start := time.Now()
	_, err = a.ReconcileOnce(ctx, pollResult.HeadSHA, st, store)
	duration := time.Since(start).Seconds()
	metrics.ReconciliationDuration.Observe(duration)

	if err != nil {
		slog.Error("reconciliation failed", "sha", pollResult.HeadSHA, "error", err)
		metrics.ReconciliationTotal.WithLabelValues("failure").Inc()

		// Track failure count for the same SHA
		if st.FailedSHA == pollResult.HeadSHA {
			st.FailedCount++
		} else {
			st.FailedSHA = pollResult.HeadSHA
			st.FailedCount = 1
		}
		st.FailedAt = time.Now()
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving failed state", "error", saveErr)
		}
		return err
	}

	metrics.ReconciliationTotal.WithLabelValues("success").Inc()
	metrics.LastSuccessfulReconciliation.SetToCurrentTime()
	return nil
}

func (a *Agent) loadAndResolve() ([]resolver.ResolvedFile, *config.Config, *resolver.Resolver, error) {
	repoFS := os.DirFS(a.repoPath)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading config: %w", err)
	}

	secretRoot, err := os.OpenRoot(a.cfg.SecretsDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening secrets dir: %w", err)
	}
	defer secretRoot.Close()

	secretReader := func(path string) (string, error) {
		data, err := secretRoot.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading secret %q: %w", path, err)
		}
		return string(data), nil
	}

	r, err := resolver.New(resolver.Config{
		FS:           repoFS,
		Config:       cfg,
		SecretReader: secretReader,
		Rootless:     a.cfg.Rootless,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(a.cfg.Hostname)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolving host %s: %w", a.cfg.Hostname, err)
	}
	return resolved.Files, cfg, r, nil
}

// ReconcileResult contains the outcome of a single reconciliation cycle.
type ReconcileResult struct {
	// HasChanges is true if any non-noop changes were applied.
	HasChanges bool
	// Summary counts changes per action type.
	Summary map[reconciler.Action]int
	// ApplyResult contains details from the apply phase (nil when no changes).
	ApplyResult *applier.ApplyResult
}

// ReconcileOnce runs a single reconciliation cycle: load config, resolve, diff, validate, apply, save state.
func (a *Agent) ReconcileOnce(ctx context.Context, headSHA string, st *state.State, store *state.Store) (*ReconcileResult, error) {
	files, cfg, r, err := a.loadAndResolve()
	if err != nil {
		return nil, err
	}

	changeset := a.computeDiff(ctx, files, st)

	if !changeset.HasChanges() {
		return &ReconcileResult{Summary: changeset.Summary}, a.markApplied(headSHA, st, store)
	}

	slog.Info("changes detected",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	v := validator.New()
	if err := v.ValidateAll(ctx, r, cfg); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	applyResult, err := a.applyWithRollback(ctx, headSHA, changeset)
	if err != nil {
		return nil, err
	}
	for _, e := range applyResult.Errors {
		slog.Warn("non-fatal apply error", "error", e)
	}

	a.updateState(headSHA, st, changeset)

	if err := store.Save(st); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	if applyResult.NeedsSelfRestart && !a.dryRun {
		slog.Info("picolet.container changed, self-update pending")
		metrics.SelfUpdatePending.Set(1)
	}

	return &ReconcileResult{
		HasChanges:  true,
		Summary:     changeset.Summary,
		ApplyResult: applyResult,
	}, nil
}

func (a *Agent) computeDiff(ctx context.Context, files []resolver.ResolvedFile, st *state.State) *reconciler.Changeset {
	rec := reconciler.New()
	var secretChecker reconciler.SecretChecker
	if a.podman != nil {
		secretChecker = func(name string) (bool, error) {
			return a.podman.SecretExists(ctx, name)
		}
	}
	return rec.Diff(files, st, secretChecker)
}

func (a *Agent) markApplied(headSHA string, st *state.State, store *state.Store) error {
	slog.Info("no changes to apply", "sha", headSHA)
	st.MarkApplied(headSHA)
	return store.Save(st)
}

func (a *Agent) applyWithRollback(ctx context.Context, headSHA string, changeset *reconciler.Changeset) (*applier.ApplyResult, error) {
	rollbackMgr := rollback.New(a.writer, a.systemd)
	snap, err := rollbackMgr.Create(changeset, os.ReadFile)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	if err := os.WriteFile(a.lockPath, []byte(headSHA), 0o600); err != nil {
		slog.Warn("writing reconciliation lock failed", "path", a.lockPath, "error", err)
	}

	app := applier.New(a.systemd, a.podman, a.writer, a.dryRun)
	result, err := app.Apply(ctx, changeset)
	if err != nil {
		slog.Error("apply failed, rolling back", "error", err)
		metrics.RollbackTotal.Inc()

		// Use a detached context so rollback can complete even during shutdown.
		// WithoutCancel preserves parent values (e.g. trace IDs) without inheriting cancellation.
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rollbackCancel()

		if rbErr := rollbackMgr.Restore(rollbackCtx, snap); rbErr != nil {
			slog.Error("rollback failed", "error", rbErr)
		}
		return nil, fmt.Errorf("apply: %w", err)
	}

	slog.Info("apply complete",
		"applied", result.Applied,
		"restarted", result.RestartedUnits,
		"dry_run", a.dryRun,
	)

	if err := os.Remove(a.lockPath); err != nil {
		slog.Warn("removing reconciliation lock failed", "path", a.lockPath, "error", err)
	}

	return result, nil
}

func (a *Agent) updateState(headSHA string, st *state.State, changeset *reconciler.Changeset) {
	st.MarkApplied(headSHA)
	st.ManagedFiles = make(map[string]string)
	for _, change := range changeset.Changes {
		if change.Action == reconciler.ActionDelete {
			continue
		}
		st.ManagedFiles[change.DestPath] = change.NewHash
	}

	metrics.ManagedUnitsTotal.Set(float64(len(st.ManagedFiles)))
	metrics.AppliedGitSHA.Reset()
	metrics.AppliedGitSHA.WithLabelValues(headSHA).Set(1)
}

func (a *Agent) serveMetrics(ctx context.Context) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.MetricsPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("metrics server shutdown error", "error", err)
		}
	}()

	slog.Info("metrics server starting", "port", a.cfg.MetricsPort)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("metrics server error", "error", err)
	}
}
