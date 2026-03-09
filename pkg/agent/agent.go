package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/orphan"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/rollback"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

const (
	defaultRepoPath = "/var/lib/picolet/repo"
	defaultLockPath = "/var/lib/picolet/reconciliation.lock"

	// DefaultStatePath is the default location for the reconciliation state file.
	// Exported so that CLI subcommands (e.g. dry-run) can read from the same path.
	DefaultStatePath = "/var/lib/picolet/state.json"
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

	webhookCh chan struct{}
	ready     atomic.Bool
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
		statePath: DefaultStatePath,
		lockPath:  defaultLockPath,
		webhookCh: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

//nolint:cyclop // sequential startup steps + select loop; splitting reduces readability
func (a *Agent) Run(ctx context.Context) error {
	// Start HTTP server (metrics, health, webhook)
	go a.serveHTTP(ctx)

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

	a.scanOrphans(ctx, store)

	slog.Info("agent started",
		"hostname", a.cfg.Hostname,
		"poll_interval", a.cfg.PollInterval,
		"dry_run", a.dryRun,
	)

	// Run first tick immediately
	if err := a.tick(ctx, poller, store, healthChecker); err != nil {
		slog.Error("initial reconciliation failed", "error", err)
	} else {
		a.ready.Store(true)
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
			} else if !a.ready.Load() {
				a.ready.Store(true)
			}
		case <-a.webhookCh:
			slog.Info("webhook-triggered reconciliation")
			if err := a.tick(ctx, poller, store, healthChecker); err != nil {
				slog.Error("webhook-triggered reconciliation failed", "error", err)
			} else if !a.ready.Load() {
				a.ready.Store(true)
			}
			ticker.Reset(a.cfg.PollInterval)
		}
	}
}

//nolint:cyclop,funlen // multiple early-returns are clearer than restructuring
func (a *Agent) tick(ctx context.Context, poller *gitpoll.Poller, store *state.Store, healthChecker *health.Checker) error {
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// Seed managed-files metrics from state on every tick
	metrics.ManagedFilesTotal.Set(float64(len(st.ManagedFiles)))
	metrics.FailedSHAConsecutiveCount.Set(float64(st.FailedCount))
	setFilesManagedMetric(countCategoriesFromState(st.ManagedFiles))
	if st.AppliedSHA != "" {
		metrics.AppliedGitSHA.WithLabelValues(st.AppliedSHA).Set(1)
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
		}
		for _, u := range hr.Restarted {
			metrics.HealthEnforcementTotal.WithLabelValues(u, "restart").Inc()
		}
		for _, u := range hr.Skipped {
			metrics.HealthEnforcementTotal.WithLabelValues(u, "skip_cooldown").Inc()
		}
	}

	// 2. Git poll
	pollResult, err := poller.Poll(ctx, st.AppliedSHA)
	if err != nil {
		metrics.GitPollTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("polling git: %w", err)
	}

	if !pollResult.Changed {
		metrics.GitPollTotal.WithLabelValues("noop").Inc()
		slog.Info("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "no_git_changes")
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		return nil
	}
	metrics.GitPollTotal.WithLabelValues("changed").Inc()

	// Skip only after >= 3 consecutive failures on the same SHA
	const maxRetries = 3
	if pollResult.HeadSHA == st.FailedSHA && st.FailedCount >= maxRetries {
		slog.Warn("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "failed_sha_gate", "failures", st.FailedCount)
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		return nil
	}

	slog.Info("new git commit detected", "sha", pollResult.HeadSHA, "prev", st.AppliedSHA)

	start := time.Now()
	_, err = a.ReconcileOnce(ctx, pollResult.HeadSHA, st, store)
	elapsed := time.Since(start)
	metrics.ReconciliationDuration.Observe(elapsed.Seconds())

	if err != nil {
		slog.Error("reconciliation failed", "sha", pollResult.HeadSHA, "error", err, "duration", elapsed.Round(time.Millisecond))
		metrics.ReconciliationTotal.WithLabelValues("failure").Inc()

		// Track failure count for the same SHA
		if st.FailedSHA == pollResult.HeadSHA {
			st.FailedCount++
		} else {
			st.FailedSHA = pollResult.HeadSHA
			st.FailedCount = 1
		}
		st.FailedAt = time.Now()
		metrics.FailedSHAConsecutiveCount.Set(float64(st.FailedCount))
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving failed state", "error", saveErr)
		}
		return err
	}

	metrics.ReconciliationTotal.WithLabelValues("success").Inc()
	metrics.LastSuccessfulReconciliation.SetToCurrentTime()
	slog.Info("reconciliation complete", "sha", pollResult.HeadSHA, "result", "success", "duration", elapsed.Round(time.Millisecond))
	return nil
}

func (a *Agent) loadAndResolve() ([]resolver.ResolvedFile, error) {
	slog.Debug("loading fleet config", "repo", a.repoPath)
	repoFS := os.DirFS(a.repoPath)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	secretReader := func(path string) (string, error) {
		secretRoot, err := os.OpenRoot(a.cfg.SecretsDir)
		if err != nil {
			return "", fmt.Errorf("opening secrets dir: %w", err)
		}
		defer secretRoot.Close()

		data, err := secretRoot.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading secret %q: %w", path, err)
		}
		return string(data), nil
	}

	slog.Debug("resolving host", "hostname", a.cfg.Hostname)
	loadStart := time.Now()
	r, err := resolver.New(resolver.Config{
		FS:           repoFS,
		Config:       cfg,
		SecretReader: secretReader,
		Rootless:     a.cfg.Rootless,
	})
	if err != nil {
		return nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(a.cfg.Hostname)
	if err != nil {
		return nil, fmt.Errorf("resolving host %s: %w", a.cfg.Hostname, err)
	}
	slog.Debug("host resolved", "hostname", a.cfg.Hostname, "files", len(resolved.Files), "duration", time.Since(loadStart).Round(time.Millisecond))
	return resolved.Files, nil
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
	files, err := a.loadAndResolve()
	if err != nil {
		return nil, err
	}

	changeset := reconciler.Diff(files, st)

	if !changeset.HasChanges() {
		slog.Info("no changes to apply", "sha", headSHA)
		markAppliedWithMetrics(st, headSHA)
		if err := store.Save(st); err != nil {
			return nil, fmt.Errorf("saving state: %w", err)
		}
		return &ReconcileResult{HasChanges: false, Summary: changeset.Summary}, nil
	}

	slog.Info("changes detected",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	if err := validator.ValidateFiles(files, a.cfg.Rootless); err != nil {
		slog.Warn("validation failed", "error", err)
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
	}

	return &ReconcileResult{
		HasChanges:  true,
		Summary:     changeset.Summary,
		ApplyResult: applyResult,
	}, nil
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
		} else {
			slog.Warn("rollback complete", "sha", headSHA)
		}
		return nil, fmt.Errorf("apply: %w", err)
	}

	for _, change := range changeset.Changes {
		if change.Action == reconciler.ActionNoop {
			continue
		}
		metrics.FilesAppliedTotal.WithLabelValues(string(change.Action), change.Category).Inc()
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
	markAppliedWithMetrics(st, headSHA)
	st.ManagedFiles = make(map[string]state.ManagedFile)
	st.ServiceNames = make(map[string]string)
	for _, change := range changeset.Changes {
		if change.Action == reconciler.ActionDelete {
			continue
		}
		st.ManagedFiles[change.DestPath] = state.ManagedFile{Hash: change.NewHash, Category: change.Category}
		if change.ServiceName != "" {
			st.ServiceNames[change.DestPath] = change.ServiceName
		}
	}

	metrics.ManagedFilesTotal.Set(float64(len(st.ManagedFiles)))
}

// markAppliedWithMetrics records a successful SHA application in both state and metrics.
func markAppliedWithMetrics(st *state.State, headSHA string) {
	prevSHA := st.AppliedSHA
	st.MarkApplied(headSHA)
	metrics.FailedSHAConsecutiveCount.Set(0)
	// Set new SHA before deleting old: a scrape during the gap sees both (harmless
	// for an info metric) rather than zero.
	metrics.AppliedGitSHA.WithLabelValues(headSHA).Set(1)
	if prevSHA != "" && prevSHA != headSHA {
		metrics.AppliedGitSHA.DeleteLabelValues(prevSHA)
	}
}

// setFilesManagedMetric overwrites FilesManagedTotal for every known category.
// Because the label set is fixed, each call is a pure Set — no Reset() needed,
// so a concurrent Prometheus scrape never sees zero or partial values.
func setFilesManagedMetric(counts map[string]float64) {
	for _, cat := range reconciler.Categories() {
		metrics.FilesManagedTotal.WithLabelValues(cat).Set(counts[cat])
	}
}

func countCategoriesFromState(managed map[string]state.ManagedFile) map[string]float64 {
	counts := make(map[string]float64, len(reconciler.Categories()))
	for _, mf := range managed {
		counts[mf.Category]++
	}
	return counts
}

// scanOrphans detects and removes files/secrets placed by a previous picolet run that
// are no longer tracked in state. Runs once at startup; errors are logged, not fatal.
func (a *Agent) scanOrphans(ctx context.Context, store *state.Store) {
	if a.dryRun {
		return
	}
	quadletDir, systemdDir, dataDir, err := resolver.ResolveDirs(a.cfg.Rootless)
	if err != nil {
		slog.Warn("resolving dirs for orphan scan failed", "error", err)
		return
	}
	st, err := store.Load()
	if err != nil {
		slog.Warn("loading state for orphan scan failed", "error", err)
		return
	}
	if len(st.ManagedFiles) == 0 {
		slog.Info("orphan scan skipped: no managed files in state")
		return
	}
	scanner := orphan.New(a.writer, a.podman, quadletDir, systemdDir, dataDir)
	result, err := scanner.Scan(ctx, st.ManagedFiles)
	if err != nil {
		slog.Warn("orphan scan error", "error", err)
	}
	if result.FilesRemoved > 0 || result.SecretsRemoved > 0 {
		slog.Info("orphan scan complete", "removed_files", result.FilesRemoved, "removed_secrets", result.SecretsRemoved)
		metrics.OrphansRemovedTotal.WithLabelValues("file").Add(float64(result.FilesRemoved))
		metrics.OrphansRemovedTotal.WithLabelValues("secret").Add(float64(result.SecretsRemoved))
	}
	if result.FilesRemoved > 0 {
		if err := a.systemd.DaemonReload(ctx); err != nil {
			slog.Warn("daemon-reload after orphan cleanup failed", "error", err)
		}
	}
}

func (a *Agent) triggerReconcile() {
	select {
	case a.webhookCh <- struct{}{}:
	default:
		// channel full — reconciliation already pending
	}
}

func (a *Agent) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !a.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/webhook", webhookHandler(a.triggerReconcile, a.cfg.WebhookSecretPath))
	return mux
}

func (a *Agent) serveHTTP(ctx context.Context) {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.MetricsPort),
		Handler:           a.newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http server shutdown error", "error", err)
		}
	}()

	slog.Info("http server starting", "port", a.cfg.MetricsPort)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("http server error", "error", err)
	}
}
