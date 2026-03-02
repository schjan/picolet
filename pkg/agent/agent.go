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

// New creates a new Agent.
func New(cfg *agentcfg.Config, opts ...Option) *Agent {
	a := &Agent{
		cfg:       cfg,
		repoPath:  defaultRepoPath,
		statePath: defaultStatePath,
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
	if _, err := os.Stat(lockPath); err == nil {
		slog.Warn("stale reconciliation lock found, will force full reconciliation")
		os.Remove(lockPath)
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

	// Skip if this SHA previously failed
	if pollResult.HeadSHA == st.FailedSHA {
		slog.Warn("skipping previously failed SHA", "sha", pollResult.HeadSHA)
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		return nil
	}

	slog.Info("new git commit detected", "sha", pollResult.HeadSHA, "prev", st.AppliedSHA)

	start := time.Now()
	err = a.reconcile(ctx, pollResult.HeadSHA, st, store)
	duration := time.Since(start).Seconds()
	metrics.ReconciliationDuration.Observe(duration)

	if err != nil {
		slog.Error("reconciliation failed", "sha", pollResult.HeadSHA, "error", err)
		metrics.ReconciliationTotal.WithLabelValues("failure").Inc()

		// Mark as failed
		st.FailedSHA = pollResult.HeadSHA
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving failed state", "error", saveErr)
		}
		return err
	}

	metrics.ReconciliationTotal.WithLabelValues("success").Inc()
	metrics.LastSuccessfulReconciliation.SetToCurrentTime()
	return nil
}

func (a *Agent) reconcile(ctx context.Context, headSHA string, st *state.State, store *state.Store) error {
	// 3. Load config + resolve host from cloned repo
	repoFS := os.DirFS(a.repoPath)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	r := resolver.New(repoFS, cfg)
	resolved, err := r.ResolveHost(a.cfg.Hostname)
	if err != nil {
		return fmt.Errorf("resolving host %s: %w", a.cfg.Hostname, err)
	}

	// 4. Diff desired vs actual
	rec := reconciler.New()
	var secretChecker reconciler.SecretChecker
	if a.podman != nil {
		secretChecker = func(name string) (bool, error) {
			return a.podman.SecretExists(ctx, name)
		}
	}
	changeset := rec.Diff(resolved.Files, st, os.ReadFile, secretChecker)

	if !changeset.HasChanges() {
		slog.Info("no changes to apply", "sha", headSHA)
		st.AppliedSHA = headSHA
		st.AppliedAt = time.Now()
		st.FailedSHA = ""
		return store.Save(st)
	}

	slog.Info("changes detected",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	// 5. Validate (defense-in-depth)
	v := validator.New()
	if err := v.ValidateAll(ctx, r, cfg); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// 6. Snapshot current files
	rollbackMgr := rollback.New(a.writer, a.systemd)
	snap, err := rollbackMgr.Create(changeset, os.ReadFile)
	if err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}

	// Write reconciliation lock
	os.WriteFile(lockPath, []byte(headSHA), 0o644)

	// 7. Apply in phased order
	app := applier.New(a.systemd, a.podman, a.writer, a.dryRun)
	result, err := app.Apply(ctx, changeset)
	if err != nil {
		slog.Error("apply failed, rolling back", "error", err)
		metrics.RollbackTotal.Inc()
		if rbErr := rollbackMgr.Restore(ctx, snap); rbErr != nil {
			slog.Error("rollback failed", "error", rbErr)
		}
		return fmt.Errorf("apply: %w", err)
	}

	slog.Info("apply complete",
		"applied", result.Applied,
		"restarted", result.RestartedUnits,
		"dry_run", a.dryRun,
	)

	// 8. Update state
	st.AppliedSHA = headSHA
	st.AppliedAt = time.Now()
	st.FailedSHA = ""
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

	// Remove lock
	os.Remove(lockPath)

	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	// 9. Self-restart if picolet.container changed
	if result.NeedsSelfRestart && !a.dryRun {
		slog.Info("picolet.container changed, self-update pending")
		metrics.SelfUpdatePending.Set(1)
		// systemd will restart us via the unit restart
	}

	return nil
}

func (a *Agent) serveMetrics(ctx context.Context) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", a.cfg.MetricsPort),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	slog.Info("metrics server starting", "port", a.cfg.MetricsPort)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("metrics server error", "error", err)
	}
}
