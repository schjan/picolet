package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/mqtt"
	op "github.com/schjan/picolet/pkg/onepassword"
	"github.com/schjan/picolet/pkg/orphan"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/rollback"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
	"github.com/schjan/picolet/pkg/version"
)

// MQTTClient provides MQTT-based pause, trigger, and status publishing.
type MQTTClient interface {
	Start(ctx context.Context, pauseFlag *atomic.Bool, triggerFn func()) error
	PublishStatus(ctx context.Context, status mqtt.Status) error
	Close(ctx context.Context)
}

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

	opReader          resolver.OpSecretReader // nil when 1Password not configured; initialized in Run
	lastOPRefresh     time.Time               // zero = never refreshed; in-memory only (restart always re-fetches)
	opRefreshInterval time.Duration           // copied from config; 0 = feature disabled

	webhookCh          chan struct{}
	ready              atomic.Bool
	paused             atomic.Bool // set by MQTT pause subscription
	seededSuccessfulAt atomic.Bool // guards one-time gauge seed from persisted state
	mqttClient         MQTTClient  // nil when MQTT not configured
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

// WithMQTT sets the MQTTClient for pause/trigger/status publishing.
func WithMQTT(c MQTTClient) Option {
	return func(a *Agent) { a.mqttClient = c }
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

//nolint:cyclop,funlen // sequential startup steps + select loop; splitting reduces readability
func (a *Agent) Run(ctx context.Context) error {
	// Start HTTP server (metrics, health, webhook)
	go a.serveHTTP(ctx)

	// Start MQTT client if configured
	if a.mqttClient != nil {
		if err := a.mqttClient.Start(ctx, &a.paused, a.triggerReconcile); err != nil {
			return fmt.Errorf("starting MQTT client: %w", err)
		}
		// Use WithoutCancel so Close() can publish offline state even during shutdown.
		// Timeout prevents hanging if the broker is unreachable at shutdown.
		defer func() {
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer closeCancel()
			a.mqttClient.Close(closeCtx)
		}()
	}

	// Check for stale lock (unclean shutdown)
	if _, err := os.Stat(a.lockPath); err == nil {
		slog.Warn("stale reconciliation lock found, will force full reconciliation")
		if err := os.Remove(a.lockPath); err != nil {
			slog.Warn("removing stale lock failed", "path", a.lockPath, "error", err)
		}
	}

	// Initialize 1Password client (if configured)
	if a.cfg.OnePassword != nil {
		var err error
		a.opReader, err = op.NewReaderFromTokenFile(ctx, a.cfg.OnePassword.TokenPath)
		if err != nil {
			return fmt.Errorf("setting up 1password: %w", err)
		}
		slog.Info("1password client initialized", "token_path", a.cfg.OnePassword.TokenPath)
		a.opRefreshInterval = a.cfg.OnePassword.RefreshInterval
	}

	// Initialize git poller
	poller := gitpoll.New(a.cfg.RepoURL, a.cfg.RepoBranch, a.repoPath, a.cfg.GitTokenPath)
	if err := poller.Init(ctx); err != nil {
		return fmt.Errorf("initializing git poller: %w", err)
	}

	store := state.NewStore(a.statePath)
	healthChecker := health.New(a.systemd)

	a.scanOrphans(ctx, store)

	metrics.FeatureInfo.WithLabelValues("mqtt").Set(boolToFloat(a.mqttClient != nil))
	metrics.FeatureInfo.WithLabelValues("onepassword").Set(boolToFloat(a.opReader != nil))

	slog.Info("agent started",
		"hostname", a.cfg.Hostname,
		"poll_interval", a.cfg.PollInterval,
		"dry_run", a.dryRun,
		"version", version.Version,
		"git_sha", version.GitSHA,
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

	// Publish MQTT status at the end of every tick (success, failure, noop, or paused).
	defer func() { a.publishMQTTStatus(ctx, st, time.Now()) }()

	// Seed managed-files metrics from state on every tick
	metrics.FailedSHAConsecutiveCount.Set(float64(st.FailedCount))
	setFilesManagedMetric(countCategoriesFromState(st.ManagedFiles))
	metrics.SetAppliedSHA(st.AppliedSHA)
	// Seed once from persisted state (not every tick — prevents backward jumps when
	// noop timestamps are in-memory only and store.Load() returns the older persisted value).
	if !a.seededSuccessfulAt.Load() && !st.LastSuccessfulReconciliationAt.IsZero() {
		a.seededSuccessfulAt.Store(true)
		metrics.LastSuccessfulReconciliation.Set(float64(st.LastSuccessfulReconciliationAt.Unix()))
	}

	// 1. Health enforcement (always — units must stay healthy even when paused)
	hr, err := healthChecker.Enforce(ctx, st)
	if err != nil {
		slog.Error("health enforcement error", "error", err)
	} else {
		recordHealthMetrics(hr)
	}

	// 1b. Pause check — health ran, skip reconciliation when paused via MQTT
	if a.paused.Load() {
		slog.Debug("reconciliation paused via MQTT")
		metrics.ReconciliationTotal.WithLabelValues("paused").Inc()
		return nil
	}

	// 2. Git poll
	pollResult, err := poller.Poll(ctx, st.AppliedSHA)
	if err != nil {
		metrics.GitPollTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("polling git: %w", err)
	}

	if !pollResult.Changed {
		if !a.opRefreshDue() {
			metrics.GitPollTotal.WithLabelValues("noop").Inc()
			slog.Debug("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "no_git_changes")
			metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
			// A noop is a successful reconciliation — the agent confirmed the
			// desired state matches. Update the timestamp so dashboards and MQTT
			// reflect "last time we verified everything is OK", not just "last
			// time files changed". Only update in-memory (the defer publishes
			// MQTT, Prometheus is scraped); no state save needed for noops.
			now := time.Now()
			st.LastSuccessfulReconciliationAt = now
			metrics.LastSuccessfulReconciliation.Set(float64(now.Unix()))
			return nil
		}
		slog.Info("forcing reconciliation for 1password secret refresh", "sha", pollResult.HeadSHA)
		// Snooze the refresh timer now so that the failed-SHA gate (below) does not
		// cause opRefreshDue() to fire on every subsequent tick.
		a.lastOPRefresh = time.Now()
	}
	metrics.GitPollTotal.WithLabelValues("changed").Inc()

	// Skip only after >= 3 consecutive failures on the same SHA, with a 1-hour expiry
	// so transient failures (e.g. D-Bus reconnection) don't permanently brick the agent.
	const maxRetries = 3
	const failedSHAExpiry = 1 * time.Hour
	if pollResult.HeadSHA == st.FailedSHA && st.FailedCount >= maxRetries && time.Since(st.FailedAt) < failedSHAExpiry {
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
		if a.opReader != nil {
			metrics.OpSyncTotal.WithLabelValues("failure").Inc()
		}

		// Track failure count for the same SHA
		if st.FailedSHA == pollResult.HeadSHA {
			st.FailedCount++
		} else {
			st.FailedSHA = pollResult.HeadSHA
			st.FailedCount = 1
		}
		st.FailedAt = time.Now()
		metrics.RecordFailedSHA(st.FailedCount)
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving failed state", "error", saveErr)
		}
		return err
	}

	if a.opReader != nil {
		a.lastOPRefresh = time.Now()
		metrics.OpSyncTotal.WithLabelValues("success").Inc()
		metrics.OpLastSyncTimestamp.SetToCurrentTime()
	}
	metrics.ReconciliationTotal.WithLabelValues("success").Inc()
	slog.Info("reconciliation complete", "sha", pollResult.HeadSHA, "result", "success", "duration", elapsed.Round(time.Millisecond))
	return nil
}

// LoadAndResolve loads fleet config from repoPath and resolves the desired state for the given host.
// It is the shared implementation behind Agent.loadAndResolve and CLI subcommands (apply, dry-run).
func LoadAndResolve(ctx context.Context, repoPath, hostname, secretsDir string, rootless bool, opSecretReader resolver.OpSecretReader) ([]resolver.ResolvedFile, error) {
	slog.Debug("loading fleet config", "repo", repoPath)
	repoFS := os.DirFS(repoPath)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	secretReader := func(path string) (string, error) {
		secretRoot, err := os.OpenRoot(secretsDir)
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

	slog.Debug("resolving host", "hostname", hostname)
	loadStart := time.Now()
	r, err := resolver.New(resolver.Config{
		FS:             repoFS,
		Config:         cfg,
		SecretReader:   secretReader,
		OpSecretReader: opSecretReader,
		Rootless:       rootless,
	})
	if err != nil {
		return nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("resolving host %s: %w", hostname, err)
	}
	slog.Debug("host resolved", "hostname", hostname, "files", len(resolved.Files), "duration", time.Since(loadStart).Round(time.Millisecond))
	return resolved.Files, nil
}

func (a *Agent) loadAndResolve(ctx context.Context) ([]resolver.ResolvedFile, error) {
	fleetPath := a.repoPath
	if a.cfg.RepoSubDir != "" {
		fleetPath = filepath.Join(a.repoPath, a.cfg.RepoSubDir)
	}
	return LoadAndResolve(ctx, fleetPath, a.cfg.Hostname, a.cfg.SecretsDir, a.cfg.Rootless, a.opReader)
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
	files, err := a.loadAndResolve(ctx)
	if err != nil {
		return nil, err
	}

	a.recordOpSecretsCount(files)

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

	markAppliedWithMetrics(st, headSHA)
	UpdateState(st, changeset)

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
		if change.Action != reconciler.ActionNoop {
			metrics.FilesAppliedTotal.WithLabelValues(string(change.Action), change.Category).Inc()
		}
		// Remove health metrics for units leaving management.
		if change.Action == reconciler.ActionDelete && change.ServiceName != "" {
			metrics.UnitHealth.Delete(change.ServiceName)
		}
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

// UpdateState rebuilds ManagedFiles and ServiceNames from the changeset.
// It does NOT call MarkApplied or touch timestamps/metrics — callers must handle that
// (the agent uses markAppliedWithMetrics; CLI commands call st.MarkApplied directly).
func UpdateState(st *state.State, changeset *reconciler.Changeset) {
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
}

// markAppliedWithMetrics records a successful SHA application in both state and metrics.
func markAppliedWithMetrics(st *state.State, headSHA string) {
	st.MarkApplied(headSHA)
	metrics.RecordAppliedSHA(headSHA)
}

// setFilesManagedMetric overwrites FilesManagedTotal for every known category.
// Because the label set is fixed, each call is a pure Set — no Reset() needed,
// so a concurrent Prometheus scrape never sees zero or partial values.
func setFilesManagedMetric(counts map[string]float64) {
	for _, cat := range reconciler.Categories() {
		metrics.FilesManagedTotal.WithLabelValues(cat).Set(counts[cat])
	}
}

func recordHealthMetrics(hr *health.CheckResult) {
	for _, u := range hr.Healthy {
		metrics.HealthCheckTotal.WithLabelValues(u, "healthy").Inc()
	}
	for _, u := range hr.Unhealthy {
		metrics.HealthCheckTotal.WithLabelValues(u, "unhealthy").Inc()
	}
	for _, u := range hr.Inactive {
		metrics.HealthCheckTotal.WithLabelValues(u, "inactive").Inc()
	}
	for _, u := range hr.Restarted {
		metrics.HealthEnforcementTotal.WithLabelValues(u, "restart").Inc()
	}
	for _, u := range hr.Skipped {
		metrics.HealthEnforcementTotal.WithLabelValues(u, "skip_cooldown").Inc()
	}
	for unit, s := range hr.Statuses {
		metrics.UnitHealth.Set(unit, s.ActiveState, s.SubState)
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

// recordOpSecretsCount updates the picolet_op_secrets_synced gauge.
func (a *Agent) recordOpSecretsCount(files []resolver.ResolvedFile) {
	if a.opReader == nil {
		return
	}
	var count int
	for _, f := range files {
		if strings.HasPrefix(f.SrcPath, "op://") {
			count++
		}
	}
	metrics.OpSecretsSynced.Set(float64(count))
}

// opRefreshDue reports whether op:// secrets should be re-fetched.
// Returns true when 1Password is configured and the refresh interval has elapsed.
func (a *Agent) opRefreshDue() bool {
	if a.opReader == nil || a.opRefreshInterval == 0 {
		return false
	}
	return a.lastOPRefresh.IsZero() || time.Since(a.lastOPRefresh) >= a.opRefreshInterval
}

func (a *Agent) triggerReconcile() {
	select {
	case a.webhookCh <- struct{}{}:
	default:
		// channel full — reconciliation already pending
	}
}

func (a *Agent) publishMQTTStatus(ctx context.Context, st *state.State, tickTime time.Time) {
	if a.mqttClient == nil || st == nil {
		return
	}
	status := mqtt.Status{
		LastReconciliation:           tickTime,
		LastSuccessfulReconciliation: st.LastSuccessfulReconciliationAt,
		AppliedSHA:                   st.AppliedSHA,
		FailedCount:                  st.FailedCount,
		Paused:                       a.paused.Load(),
	}
	if err := a.mqttClient.PublishStatus(ctx, status); err != nil {
		slog.Warn("mqtt status publish failed", "error", err)
	} else {
		slog.Debug("mqtt status published", "sha", status.AppliedSHA, "paused", status.Paused)
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

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
