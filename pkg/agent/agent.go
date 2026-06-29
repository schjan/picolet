package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	pp "github.com/schjan/picolet/pkg/protonpass"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/rollback"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
	"github.com/schjan/picolet/pkg/validator"
	"github.com/schjan/picolet/pkg/version"
)

// MQTTClient provides MQTT-based pause, trigger, and status publishing.
type MQTTClient interface {
	Start(ctx context.Context, pauseFlag *atomic.Bool, triggerFn func()) error
	PublishStatus(ctx context.Context, status mqtt.Status) error
	Close(ctx context.Context)
}

// DeploymentReporter reports deployment lifecycle to an external system.
type DeploymentReporter interface {
	CreateDeployment(ctx context.Context, sha string) (int64, error)
	ReportInProgress(ctx context.Context, deploymentID int64) error
	ReportSuccess(ctx context.Context, deploymentID int64) error
	ReportFailure(ctx context.Context, deploymentID int64, err error) error
	ReportError(ctx context.Context, deploymentID int64, err error) error
}

const (
	defaultRepoPath = "/var/lib/picolet/repo"

	// DefaultLockPath is the default cross-process lock for mutating commands.
	DefaultLockPath = "/var/lib/picolet/reconciliation.lock"

	// DefaultStatePath is the default location for the reconciliation state file.
	// Exported so that CLI subcommands (e.g. dry-run) can read from the same path.
	DefaultStatePath = "/var/lib/picolet/state.json"
)

// errRollbackPerformed is returned by applyWithRollback when an apply failure
// triggered (and finished) a rollback. github.go uses it via
// shouldReportDeploymentError to distinguish rollback-after-failure from a
// plain failure when reporting GitHub Deployment status.
var errRollbackPerformed = errors.New("rollback performed")

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

	// quadletDirOverride, systemdDirOverride, and dataDirOverride are
	// test-only injection points. Empty values fall back to
	// resolver.ResolveDirs(cfg.Rootless). Production deployments leave
	// them unset so destination paths follow the documented layout.
	// loadAndResolve passes these straight to ResolveParams (resolver.New
	// owns fallback); scanOrphans applies the same fallback inline.
	quadletDirOverride string
	systemdDirOverride string
	dataDirOverride    string

	opReader resolver.SecretRefReader // nil when 1Password not configured; initialized in Run, consumed in secrets.go
	ppReader resolver.SecretRefReader // nil when Proton Pass not configured; initialized in Run, consumed in secrets.go
	// Accessed only by the agent tick loop, which runs serially.
	lastOPRefresh time.Time // zero = never refreshed; in-memory only (restart always re-fetches)
	lastPPRefresh time.Time // zero = never refreshed; in-memory only (restart always re-fetches)
	// lastPruneAttemptAt bounds image-prune retries after a failure (in-memory;
	// tick-loop only). seededPrunedAt guards the one-time metric seed from state.
	lastPruneAttemptAt time.Time
	seededPrunedAt     atomic.Bool

	webhookCh chan struct{}
	// ready: written by Run() after first successful tick; read by /health handler (http.go).
	ready atomic.Bool
	// paused: written by MQTT pause subscription (mqttClient.Start); read in tick() and by /health handler (http.go).
	paused atomic.Bool
	// seededSuccessfulAt: written and read only in tick(); guards one-time gauge seed from persisted state.
	seededSuccessfulAt atomic.Bool
	mqttClient         MQTTClient           // nil when MQTT not configured
	deployReporter     DeploymentReporter   // nil when GitHub App not configured; consumed in github.go
	authProvider       gitpoll.AuthProvider // nil = use default SSH/token logic
	// consecutiveHealthFailures: written by updateHealthFailures (http.go); read by /health handler (http.go).
	consecutiveHealthFailures atomic.Int32
	routeRegistrar            RouteRegistrar // nil = no extra HTTP routes
	statusStore               *status.Store
}

// RouteRegistrar is implemented by anything that wants to add routes to the
// agent's HTTP mux. Defined here (not in pkg/dashboard) so pkg/agent stays
// independent of UI implementation.
type RouteRegistrar interface {
	Register(*http.ServeMux)
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

// WithQuadletDir overrides the quadlet destination directory. Test-only;
// production agents leave this unset so paths come from resolver.ResolveDirs.
// An empty path is ignored. When this option is set, callers that also
// use systemd or manifest categories must set WithSystemdDir/WithDataDir
// to keep loadAndResolve and scanOrphans pointed at consistent locations.
func WithQuadletDir(path string) Option {
	return func(a *Agent) {
		if path != "" {
			a.quadletDirOverride = path
		}
	}
}

// WithSystemdDir overrides the systemd destination directory. Test-only.
// An empty path is ignored. See WithQuadletDir for the consistency rule.
func WithSystemdDir(path string) Option {
	return func(a *Agent) {
		if path != "" {
			a.systemdDirOverride = path
		}
	}
}

// WithDataDir overrides the manifest data directory. Test-only.
// An empty path is ignored. See WithQuadletDir for the consistency rule.
func WithDataDir(path string) Option {
	return func(a *Agent) {
		if path != "" {
			a.dataDirOverride = path
		}
	}
}

// WithMQTT sets the MQTTClient for pause/trigger/status publishing.
func WithMQTT(c MQTTClient) Option {
	return func(a *Agent) { a.mqttClient = c }
}

// WithDeploymentReporter sets the deployment status reporter.
func WithDeploymentReporter(r DeploymentReporter) Option {
	return func(a *Agent) { a.deployReporter = r }
}

// WithAuthProvider sets the git auth provider.
func WithAuthProvider(p gitpoll.AuthProvider) Option {
	return func(a *Agent) { a.authProvider = p }
}

// WithDashboard registers an additional RouteRegistrar (typically the
// dashboard handler) onto the agent's HTTP mux alongside /metrics, /health,
// and /webhook.
func WithDashboard(r RouteRegistrar) Option {
	return func(a *Agent) { a.routeRegistrar = r }
}

// WithStatusStore sets the shared in-memory runtime status store.
func WithStatusStore(store *status.Store) Option {
	return func(a *Agent) {
		if store != nil {
			a.statusStore = store
		}
	}
}

// New creates a new Agent.
func New(cfg *agentcfg.Config, opts ...Option) *Agent {
	a := &Agent{
		cfg:         cfg,
		repoPath:    defaultRepoPath,
		statePath:   DefaultStatePath,
		lockPath:    DefaultLockPath,
		webhookCh:   make(chan struct{}, 1),
		statusStore: status.NewStore(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

//nolint:cyclop,funlen // sequential startup steps + select loop; splitting reduces readability
func (a *Agent) Run(ctx context.Context) error {
	releaseLock, err := AcquireLock(a.lockPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := releaseLock(); err != nil {
			slog.Warn("releasing process lock failed", "path", a.lockPath, "error", err)
		}
	}()

	// Start HTTP server (metrics, health, webhook)
	shutdownHTTP, err := a.startHTTP()
	if err != nil {
		return err
	}
	defer shutdownHTTP(ctx)

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

	// Initialize 1Password client (if configured)
	if a.cfg.OnePassword != nil {
		var err error
		a.opReader, err = op.NewReaderFromTokenFile(ctx, a.cfg.OnePassword.TokenPath)
		if err != nil {
			return fmt.Errorf("setting up 1password: %w", err)
		}
		slog.Info("1password client initialized", "token_path", a.cfg.OnePassword.TokenPath)
		publishCredentialExpiry(resolver.ProviderOnePassword, a.cfg.OnePassword.TokenExpiresAt)
	}

	// Bound pp.NewReader's EnsureSession with a deadline so a hung login can't block startup.
	if a.cfg.ProtonPass != nil {
		initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		reader, err := pp.NewReader(initCtx, a.cfg.ProtonPass.ToClientConfig())
		cancel()
		if err != nil {
			return fmt.Errorf("setting up protonpass: %w", err)
		}
		a.ppReader = reader
		slog.Info("protonpass client initialized",
			"session_dir", a.cfg.ProtonPass.SessionDir,
			"lazy_mode", a.cfg.ProtonPass.PATPath == "",
		)
		publishCredentialExpiry(resolver.ProviderProtonPass, a.cfg.ProtonPass.PATExpiresAt)
	}

	// Required invariant: opReader/ppReader must be non-nil here before resolvePollerAuth()
	// (defined in secrets.go) is called below. The secrets.go helpers read these fields and
	// silently fall through to SSH/file-based auth if nil — moving this init block later, or
	// reordering it past resolvePollerAuth, will produce a quiet auth misroute.

	// Initialize git poller
	auth, err := a.resolvePollerAuth(ctx)
	if err != nil {
		return err
	}
	poller := gitpoll.New(a.cfg.RepoURL, a.cfg.RepoBranch, a.repoPath, auth)
	if err := poller.Init(ctx); err != nil {
		return fmt.Errorf("initializing git poller: %w", err)
	}

	store := state.NewStore(a.statePath)
	healthChecker := health.New(a.systemd)

	a.scanOrphans(ctx, store)

	metrics.FeatureInfo.WithLabelValues("mqtt").Set(boolToFloat(a.mqttClient != nil))
	metrics.FeatureInfo.WithLabelValues(string(resolver.ProviderOnePassword)).Set(boolToFloat(a.opReader != nil))
	metrics.FeatureInfo.WithLabelValues(string(resolver.ProviderProtonPass)).Set(boolToFloat(a.ppReader != nil))

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

	// Snapshot PendingUnits so a health-enforce mutation can be persisted even
	// on tick paths (noop, paused) that would otherwise not save state.
	pendingUnitsBefore := maps.Clone(st.PendingUnits)

	// Publish MQTT status at the end of every tick (success, failure, noop, or paused).
	defer func() { a.publishMQTTStatus(ctx, st, time.Now()) }()
	// Refresh the per-unit restart-pending metric from the final state at tick end.
	defer func() { metrics.SetUnitRestartPending(pendingUnitAttempts(st.PendingUnits)) }()

	// Seed managed-files metrics from state on every tick
	metrics.FailedSHAConsecutiveCount.Set(float64(st.FailedCount))
	managedByCategory := make(map[string]float64, len(reconciler.Categories()))
	for _, mf := range st.ManagedFiles {
		managedByCategory[mf.Category.String()]++
	}
	setFilesManagedMetric(managedByCategory)
	metrics.SetAppliedSHA(st.AppliedSHA)
	// Seed once from persisted state (not every tick — prevents backward jumps when
	// noop timestamps are in-memory only and store.Load() returns the older persisted value).
	if !a.seededSuccessfulAt.Load() && !st.LastSuccessfulReconciliationAt.IsZero() {
		a.seededSuccessfulAt.Store(true)
		metrics.LastSuccessfulReconciliation.Set(float64(st.LastSuccessfulReconciliationAt.Unix()))
	}
	// Seed the last-prune timestamp once from persisted state so the metric
	// survives a restart (the status store is in-memory and starts empty).
	if !a.seededPrunedAt.Load() && !st.LastPrunedAt.IsZero() {
		a.seededPrunedAt.Store(true)
		a.statusStore.SetPrune(status.PruneStatus{LastRunAt: st.LastPrunedAt})
	}

	// 1. Health enforcement (always — units must stay healthy even when paused)
	hr, err := healthChecker.Enforce(ctx, st)
	if err != nil {
		slog.Error("health enforcement error", "error", err)
	} else {
		a.recordHealthMetrics(hr)
	}

	// Track consecutive D-Bus failures for /health endpoint.
	// AllFailed() is true only when every GetUnitStatus call errored — the signal
	// for D-Bus being dead (RestartUnit errors don't count because those units
	// already appeared in Unhealthy).
	if hr != nil {
		a.updateHealthFailures(hr)
	}

	// Persist any PendingUnits change health-enforce made (clear on convergence,
	// increment on a failed retry, prune removed units). Done here — before the
	// pause check and the noop fast-path — because those paths return without
	// otherwise saving state, which would lose the attempt count and cooldown.
	//
	// This maps.Equal compares PendingUnit values, time.Time fields included. It
	// is reliable only because every write site — mergePendingUnits and
	// health.recordPendingUnit — truncates timestamps to whole seconds, which
	// strips the monotonic reading so a value survives a persist/reload round
	// trip unchanged. A future write site that skips that truncation would
	// silently break this change detection.
	if !maps.Equal(st.PendingUnits, pendingUnitsBefore) {
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving state after health enforcement", "error", saveErr)
		}
	}

	// 1b. Pause check — health ran, skip reconciliation when paused via MQTT
	if a.paused.Load() {
		slog.Debug("reconciliation paused via MQTT")
		metrics.ReconciliationTotal.WithLabelValues("paused").Inc()
		a.recordEvent("paused", st.AppliedSHA, "reconciliation paused via MQTT")
		return nil
	}

	// 1c. Maintenance: prune unused images when due. Placed inside tick() so it
	// is strictly serialized against ReconcileOnce/apply (same goroutine); after
	// the pause check so it respects an MQTT pause; before the noop/failed-SHA
	// early returns so it still runs on the common no-change tick path.
	a.maybePruneImages(ctx, st, store)

	// 2. Git poll
	pollResult, err := poller.Poll(ctx, st.AppliedSHA)
	if err != nil {
		metrics.GitPollTotal.WithLabelValues("error").Inc()
		// SHA intentionally empty: the failure is about the upstream poll, not
		// about the currently-applied SHA, which would be misleading.
		a.recordEvent("git_error", "", err.Error())
		return fmt.Errorf("polling git: %w", err)
	}

	if !pollResult.Changed && !a.opRefreshDue() && !a.ppRefreshDue() && len(st.PendingHooks) == 0 && len(st.PendingUnits) == 0 {
		metrics.GitPollTotal.WithLabelValues("noop").Inc()
		slog.Debug("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "no_git_changes")
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		if !a.statusStore.Snapshot().Bootstrapped {
			if err := a.refreshResolvedSnapshot(ctx); err != nil {
				slog.Warn("refreshing runtime status snapshot failed", "error", err)
				a.recordEvent("status_error", pollResult.HeadSHA, err.Error())
			}
		}
		// A noop is a successful reconciliation — the agent confirmed the
		// desired state matches. Update the timestamp so dashboards and MQTT
		// reflect "last time we verified everything is OK", not just "last
		// time files changed". Only update in-memory (the defer publishes
		// MQTT, Prometheus is scraped); no state save needed for noops.
		now := time.Now()
		st.LastSuccessfulReconciliationAt = now
		metrics.LastSuccessfulReconciliation.Set(float64(now.Unix()))
		a.statusStore.SetVerifiedAt(now)
		return nil
	}

	// Pending-unit retry tick: nothing in git changed and no hook/secret retry
	// is due, but units are still failing to restart. Health-enforce already
	// retried them this tick (and persisted any change above). Report
	// retry_pending — NOT a clean noop — so the SHA is not treated as fully
	// converged and LastSuccessfulReconciliationAt is not advanced. Units are
	// retried by health-enforce, so no ReconcileOnce is needed here.
	if !pollResult.Changed && !a.opRefreshDue() && !a.ppRefreshDue() && len(st.PendingHooks) == 0 {
		metrics.GitPollTotal.WithLabelValues("pending_unit_retry").Inc()
		metrics.ReconciliationTotal.WithLabelValues("retry_pending").Inc()
		slog.Warn("apply incomplete, units still failing to restart",
			"sha", pollResult.HeadSHA, "pending_units", slices.Sorted(maps.Keys(st.PendingUnits)))
		a.recordEvent("retry_pending", st.AppliedSHA, "units still failing to restart")
		return nil
	}

	switch {
	case pollResult.Changed:
		metrics.GitPollTotal.WithLabelValues("changed").Inc()
	case len(st.PendingHooks) > 0:
		// Pending-hook retry takes priority over secret-provider refresh in the
		// label even when both apply: the retry is the actionable reason this
		// tick ran, and ReconcileOnce will refresh secrets regardless.
		slog.Info("forcing reconciliation for pending hook retry", "sha", pollResult.HeadSHA, "pending", st.PendingHooks)
		metrics.GitPollTotal.WithLabelValues("pending_hook_retry").Inc()
	default:
		slog.Info("forcing reconciliation for secret-provider refresh", "sha", pollResult.HeadSHA)
		metrics.GitPollTotal.WithLabelValues("secret_refresh").Inc()
	}

	// Skip only after >= 3 consecutive failures on the same SHA, with a 1-hour expiry
	// so transient failures (e.g. D-Bus reconnection) don't permanently brick the agent.
	const maxRetries = 3
	const failedSHAExpiry = 1 * time.Hour
	if pollResult.HeadSHA == st.FailedSHA && st.FailedCount >= maxRetries && time.Since(st.FailedAt) < failedSHAExpiry {
		slog.Warn("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "failed_sha_gate", "failures", st.FailedCount)
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		// Bump every provider's last-refresh timestamp so the agent does not
		// retry secret refreshes every tick while blocked by the failed-SHA gate.
		a.markRefreshAttempted()
		a.recordEvent("failed_sha_gate", pollResult.HeadSHA, "skipped after repeated failures")
		return nil
	}

	// Report deployments only for new git SHAs; forced reconciliations on the
	// same SHA (secret-provider refresh, pending hook retry) are not deployments.
	var deploymentID int64
	if pollResult.Changed {
		slog.Info("new git commit detected", "sha", pollResult.HeadSHA, "prev", st.AppliedSHA)
		deploymentID = a.createDeployment(ctx, pollResult.HeadSHA)
	}

	start := time.Now()
	result, err := a.ReconcileOnce(ctx, pollResult.HeadSHA, st, store)
	elapsed := time.Since(start)
	metrics.ReconciliationDuration.Observe(elapsed.Seconds())

	if errors.Is(err, applier.ErrApplyIncomplete) {
		// Files were applied; only hook retry is pending. This is not a
		// reconciliation failure: do not increment FailedCount, do not arm the
		// failed-SHA gate, do not report a failed deployment.
		a.reportDeploymentResult(ctx, deploymentID, nil)
		a.markRefreshAttempted()
		slog.Warn("apply incomplete, hooks will retry on next tick", "sha", pollResult.HeadSHA, "error", err, "duration", elapsed.Round(time.Millisecond))
		metrics.ReconciliationTotal.WithLabelValues("retry_pending").Inc()
		a.recordEvent("retry_pending", pollResult.HeadSHA, err.Error())
		return nil
	}

	a.reportDeploymentResult(ctx, deploymentID, err)

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
		metrics.RecordFailedSHA(st.FailedCount)
		a.recordEvent("failure", pollResult.HeadSHA, err.Error())
		if saveErr := store.Save(st); saveErr != nil {
			slog.Error("saving failed state", "error", saveErr)
		}
		return err
	}

	a.markRefreshAttempted()
	recordProviderSyncSuccess(resolver.ProviderOnePassword, result.OpSecretsCount)
	recordProviderSyncSuccess(resolver.ProviderProtonPass, result.PPSecretsCount)
	metrics.ReconciliationTotal.WithLabelValues("success").Inc()
	if result.HasChanges {
		a.recordEvent("success", pollResult.HeadSHA, "reconciliation complete")
	}
	slog.Info("reconciliation complete", "sha", pollResult.HeadSHA, "result", "success", "duration", elapsed.Round(time.Millisecond))
	return nil
}

// ResolveParams holds the parameters for LoadAndResolve and LoadAndResolveHost.
type ResolveParams struct {
	RepoPath       string
	Hostname       string
	SecretsDir     string
	Rootless       bool
	OpSecretReader resolver.SecretRefReader
	PPSecretReader resolver.SecretRefReader

	// QuadletDir, SystemdDir, and DataDir override the defaults computed by
	// resolver.ResolveDirs. Empty fields fall back to the default. Used by
	// tests to isolate destination paths.
	QuadletDir string
	SystemdDir string
	DataDir    string

	// HostDataDir is the host-visible data path emitted by the filePath/
	// manifestPath template helpers. Empty falls back to the internal data dir.
	HostDataDir string
}

// LoadAndResolve loads fleet config from repoPath and resolves the desired state for the given host.
// It is the shared implementation behind Agent.loadAndResolve and CLI subcommands (apply, dry-run).
func LoadAndResolve(ctx context.Context, params ResolveParams) ([]resolver.ResolvedFile, error) {
	resolved, err := LoadAndResolveHost(ctx, params)
	if err != nil {
		return nil, err
	}
	return resolved.Files, nil
}

// LoadAndResolveHost loads fleet config from repoPath and resolves the desired state plus host metadata.
func LoadAndResolveHost(ctx context.Context, params ResolveParams) (*resolver.ResolvedHost, error) {
	slog.Debug("loading fleet config", "repo", params.RepoPath)
	repoFS := os.DirFS(params.RepoPath)
	cfg, err := config.LoadAll(repoFS)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	secretReader := func(path string) (string, error) {
		secretRoot, err := os.OpenRoot(params.SecretsDir)
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

	slog.Debug("resolving host", "hostname", params.Hostname)
	loadStart := time.Now()
	r, err := resolver.New(resolver.Config{
		FS:             repoFS,
		Config:         cfg,
		SecretReader:   secretReader,
		OpSecretReader: params.OpSecretReader,
		PPSecretReader: params.PPSecretReader,
		Rootless:       params.Rootless,
		QuadletDir:     params.QuadletDir,
		SystemdDir:     params.SystemdDir,
		DataDir:        params.DataDir,
		HostDataDir:    params.HostDataDir,
	})
	if err != nil {
		return nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveHost(ctx, params.Hostname)
	if err != nil {
		return nil, fmt.Errorf("resolving host %s: %w", params.Hostname, err)
	}
	slog.Debug("host resolved", "hostname", params.Hostname, "files", len(resolved.Files), "duration", time.Since(loadStart).Round(time.Millisecond))
	return resolved, nil
}

func (a *Agent) loadAndResolve(ctx context.Context) (*resolver.ResolvedHost, error) {
	fleetPath := a.repoPath
	if a.cfg.RepoSubDir != "" {
		fleetPath = filepath.Join(a.repoPath, a.cfg.RepoSubDir)
	}
	return LoadAndResolveHost(ctx, ResolveParams{
		RepoPath:       fleetPath,
		Hostname:       a.cfg.Hostname,
		SecretsDir:     a.cfg.SecretsDir,
		Rootless:       a.cfg.Rootless,
		OpSecretReader: a.opReader,
		PPSecretReader: a.ppReader,
		// Override fields are passed raw; resolver.New applies the
		// ResolveDirs fallback for any field left empty.
		QuadletDir:  a.quadletDirOverride,
		SystemdDir:  a.systemdDirOverride,
		DataDir:     a.dataDirOverride,
		HostDataDir: a.cfg.EffectiveHostDataDir(),
	})
}

// ReconcileResult contains the outcome of a single reconciliation cycle.
type ReconcileResult struct {
	// HasChanges is true if any non-noop changes were applied.
	HasChanges bool
	// Summary counts changes per action type.
	Summary map[reconciler.Action]int
	// ApplyResult contains details from the apply phase (nil when no changes).
	ApplyResult *applier.ApplyResult
	// OpSecretsCount is the number of op:// secrets resolved in this cycle.
	OpSecretsCount int
	// PPSecretsCount is the number of pass:// secrets resolved in this cycle.
	PPSecretsCount int
}

// refSecretsCounts groups the per-provider direct-secret counts that flow
// through the reconcile helpers. Fewer parameters than (op, pp int) per call.
type refSecretsCounts struct {
	Op, PP int
}

// ReconcileOnce runs a single reconciliation cycle: load config, resolve, diff, validate, apply, save state.
func (a *Agent) ReconcileOnce(ctx context.Context, headSHA string, st *state.State, store *state.Store) (*ReconcileResult, error) {
	resolved, err := a.loadAndResolve(ctx)
	if err != nil {
		return nil, err
	}
	files := resolved.Files
	a.recordHostMetadata(resolved.Host)

	counts := refSecretsCounts{
		Op: recordProviderRefCount(resolver.ProviderOnePassword, a.opReader != nil, op.IsRef, files),
		PP: recordProviderRefCount(resolver.ProviderProtonPass, a.ppReader != nil, pp.IsRef, files),
	}

	changeset := reconciler.Diff(files, st)

	if !changeset.HasChanges() {
		if len(st.PendingHooks) > 0 {
			return a.retryPendingHooks(ctx, resolved, st, store, changeset, counts)
		}
		return a.reconcileNoChanges(headSHA, files, changeset, st, store, counts)
	}

	slog.Info("changes detected",
		"create", changeset.Summary[reconciler.ActionCreate],
		"update", changeset.Summary[reconciler.ActionUpdate],
		"delete", changeset.Summary[reconciler.ActionDelete],
	)

	deps, err := validator.AnalyzeFiles(files, a.cfg.Rootless)
	if err != nil {
		slog.Warn("validation failed", "error", err)
		a.recordEvent("failure", headSHA, fmt.Sprintf("validation failed: %v", err))
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	applyResult, err := a.applyWithRollback(ctx, headSHA, changeset, resolved.Hooks, pendingHookNames(st.PendingHooks))
	recordHookMetrics(applyResult)
	if errors.Is(err, applier.ErrApplyIncomplete) {
		a.savePartialState(headSHA, st, store, changeset, applyResult, deps, resolved.Hooks)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return a.finalizeApply(headSHA, st, store, changeset, applyResult, deps, counts, resolved.Hooks)
}

func (a *Agent) finalizeApply(headSHA string, st *state.State, store *state.Store, changeset *reconciler.Changeset, applyResult *applier.ApplyResult, deps map[string]status.UnitDependencies, counts refSecretsCounts, hooks []config.Hook) (*ReconcileResult, error) {
	// Set deps after a successful apply so the dashboard's dep map matches the
	// deployed state. On apply failure (rollback) we keep the previous good
	// map rather than advertising deps for a state we couldn't reach.
	a.statusStore.SetDependencies(deps)
	st.PendingHooks = enforceRetryBudget(mergePendingHooks(st.PendingHooks, applyResult), hooks)
	logNonRetryableApplyErrors(applyResult)

	markAppliedWithMetrics(st, headSHA)
	a.statusStore.SetVerifiedAt(st.LastSuccessfulReconciliationAt)
	UpdateState(st, changeset)
	// Reconcile pending-unit records: clear units that restarted cleanly, seed
	// any that failed, then prune records for units removed from the fleet.
	st.PendingUnits = prunePendingUnits(mergePendingUnits(st.PendingUnits, applyResult, headSHA, time.Now()), st.ServiceNames)

	if err := store.Save(st); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	if applyResult.NeedsSelfRestart && !a.dryRun {
		slog.Info("picolet.container changed, self-update pending")
	}

	return &ReconcileResult{
		HasChanges:     true,
		Summary:        changeset.Summary,
		ApplyResult:    applyResult,
		OpSecretsCount: counts.Op,
		PPSecretsCount: counts.PP,
	}, nil
}

// reconcileNoChanges handles a reconcile that found no file changes.
// When the git SHA is new, it still marks that SHA as applied and persists it:
// a host can legitimately receive a fleet commit that changes only docs or
// other hosts. When the SHA is unchanged (e.g. secret-provider refresh), it
// only bumps the verified-OK timestamp in memory.
//
// Note on validation failure: this path treats it as a hard failure and
// surfaces it through the failure-event/failed-SHA pipeline. The outer-tick
// noop fast path (no_git_changes) is more lenient: validation errors there
// during refreshResolvedSnapshot are logged and recorded as status_error
// without aborting the verify-OK signal, because nothing triggered the tick.
// Here, something *did* trigger the tick (secret-provider refresh or git
// change that rendered identically), so a bad render is actionable.
func (a *Agent) reconcileNoChanges(headSHA string, files []resolver.ResolvedFile, changeset *reconciler.Changeset, st *state.State, store *state.Store, counts refSecretsCounts) (*ReconcileResult, error) {
	deps, err := validator.AnalyzeFiles(files, a.cfg.Rootless)
	if err != nil {
		a.recordEvent("failure", headSHA, fmt.Sprintf("validation failed: %v", err))
		return nil, fmt.Errorf("validation failed: %w", err)
	}
	a.statusStore.SetDependencies(deps)
	slog.Info("no changes to apply", "sha", headSHA)
	if headSHA != st.AppliedSHA {
		markAppliedWithMetrics(st, headSHA)
		a.statusStore.SetVerifiedAt(st.LastSuccessfulReconciliationAt)
		if err := store.Save(st); err != nil {
			return nil, fmt.Errorf("saving state: %w", err)
		}
		return &ReconcileResult{HasChanges: false, Summary: changeset.Summary, OpSecretsCount: counts.Op, PPSecretsCount: counts.PP}, nil
	}
	now := time.Now()
	st.LastSuccessfulReconciliationAt = now
	metrics.LastSuccessfulReconciliation.Set(float64(now.Unix()))
	a.statusStore.SetVerifiedAt(now)
	return &ReconcileResult{HasChanges: false, Summary: changeset.Summary, OpSecretsCount: counts.Op, PPSecretsCount: counts.PP}, nil
}

// savePartialState persists state after a file apply that succeeded but did not
// fully converge — a keep_running hook failed, or a unit restart failed. Files
// and secrets are recorded (so the next tick does not re-write them), the SHA is
// marked applied (so gitpoll stops reporting "Changed" on every retry tick —
// which would otherwise produce duplicate deployment reports for the same SHA),
// and the pending-hook and pending-unit records are saved so the retries survive
// an agent restart.
func (a *Agent) savePartialState(headSHA string, st *state.State, store *state.Store, changeset *reconciler.Changeset, applyResult *applier.ApplyResult, deps map[string]status.UnitDependencies, hooks []config.Hook) {
	UpdateState(st, changeset)
	st.PendingHooks = enforceRetryBudget(mergePendingHooks(st.PendingHooks, applyResult), hooks)
	// Prune against the just-rebuilt ServiceNames so a unit removed from the
	// fleet drops its pending record (health-enforce cannot see removals).
	st.PendingUnits = prunePendingUnits(mergePendingUnits(st.PendingUnits, applyResult, headSHA, time.Now()), st.ServiceNames)
	// Record the SHA without advancing the last-successful timestamp: the apply
	// is incomplete, so staleness alerts must keep firing until it converges.
	markAppliedIncompleteWithMetrics(st, headSHA)
	if saveErr := store.Save(st); saveErr != nil {
		slog.Error("saving partial state after incomplete apply", "error", saveErr)
	}
	a.statusStore.SetDependencies(deps)
}

// mergePendingHooks computes the new PendingHooks map given the previous
// map and the apply result. After the every-tick-retry change a hook either
// runs (success/failed/skipped) or is dropped as stale; it does not stay
// pending across ticks without an attempt. So a hook is removed from
// pending if it appears in AttemptedHookNames (regardless of outcome
// classification — count increments come from PendingHookNames), and added
// if it's a new keep_running failure. Returns nil (not an empty map) when
// empty so omitempty omits the field.
func mergePendingHooks(old map[string]int, result *applier.ApplyResult) map[string]int {
	if len(old) == 0 && len(result.PendingHookNames) == 0 {
		return nil
	}
	attempted := make(map[string]bool, len(result.AttemptedHookNames))
	for _, name := range result.AttemptedHookNames {
		attempted[name] = true
	}
	merged := make(map[string]int, len(old)+len(result.PendingHookNames))
	for name, count := range old {
		if attempted[name] {
			continue
		}
		merged[name] = count
	}
	for _, name := range result.PendingHookNames {
		prev := old[name]
		merged[name] = prev + 1
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// mergePendingUnits computes the new PendingUnits map from the previous map and
// an apply result: units that restarted cleanly are dropped (converged), units
// whose restart failed are added or have their attempt count incremented, and
// every other entry is carried forward unchanged. headSHA records the SHA in
// effect for any failure recorded this call. Timestamps are truncated to whole
// seconds so maps.Equal change-detection is stable. Returns nil (not an empty
// map) when empty so omitempty omits the field.
func mergePendingUnits(old map[string]state.PendingUnit, result *applier.ApplyResult, headSHA string, now time.Time) map[string]state.PendingUnit {
	if len(old) == 0 && len(result.FailedRestartUnits) == 0 {
		return nil
	}
	now = now.Truncate(time.Second)
	merged := make(map[string]state.PendingUnit, len(old)+len(result.FailedRestartUnits))
	maps.Copy(merged, old)
	for _, unit := range result.RestartedUnits {
		delete(merged, unit)
	}
	for _, unit := range result.FailedRestartUnits {
		pu := merged[unit]
		if pu.FirstFailedAt.IsZero() {
			pu.FirstFailedAt = now
		}
		pu.SHA = headSHA
		pu.Attempts++
		pu.LastAttemptAt = now
		merged[unit] = pu
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// prunePendingUnits drops pending-unit records whose unit is no longer managed
// (not present in serviceNames). Mutates and returns pending; returns nil when
// the result is empty so omitempty omits the field.
func prunePendingUnits(pending map[string]state.PendingUnit, serviceNames map[string]string) map[string]state.PendingUnit {
	if len(pending) == 0 {
		return nil
	}
	managed := make(map[string]struct{}, len(serviceNames))
	for _, unit := range serviceNames {
		managed[unit] = struct{}{}
	}
	for unit := range pending {
		if _, ok := managed[unit]; !ok {
			delete(pending, unit)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return pending
}

// pendingUnitAttempts projects PendingUnits to a unit→attempt-count map for the
// picolet_unit_restart_pending metric.
func pendingUnitAttempts(pending map[string]state.PendingUnit) map[string]int {
	if len(pending) == 0 {
		return nil
	}
	out := make(map[string]int, len(pending))
	for unit, pu := range pending {
		out[unit] = pu.Attempts
	}
	return out
}

// retryPendingHooks runs pending secret hooks on a tick where the diff is
// otherwise empty. It does NOT call UpdateState — that would wipe ManagedFiles
// from an empty changeset. State is saved with the (possibly trimmed) pending
// map. Returns ErrApplyIncomplete when hooks still fail under keep_running.
func (a *Agent) retryPendingHooks(ctx context.Context, resolved *resolver.ResolvedHost, st *state.State, store *state.Store, changeset *reconciler.Changeset, counts refSecretsCounts) (*ReconcileResult, error) {
	pendingNames := pendingHookNames(st.PendingHooks)
	app := applier.New(a.systemd, a.podman, a.writer, a.dryRun, resolved.Hooks)
	result := app.RunPendingHooks(ctx, pendingNames)

	recordHookMetrics(result)
	logNonRetryableApplyErrors(result)

	newPending := mergePendingHooks(st.PendingHooks, result)
	newPending = enforceRetryBudget(newPending, resolved.Hooks)

	if !maps.Equal(st.PendingHooks, newPending) {
		st.PendingHooks = newPending
		if err := store.Save(st); err != nil {
			return nil, fmt.Errorf("saving state after hook retry: %w", err)
		}
	}

	out := &ReconcileResult{
		HasChanges:     len(result.RestartedUnits) > 0 || len(result.FallbackRestartedUnits) > 0,
		Summary:        changeset.Summary,
		ApplyResult:    result,
		OpSecretsCount: counts.Op,
		PPSecretsCount: counts.PP,
	}

	if len(result.RetryableErrors) > 0 {
		return out, fmt.Errorf("%w: %w", applier.ErrApplyIncomplete, errors.Join(result.RetryableErrors...))
	}

	slog.Info("pending hooks retried", "restarted", result.RestartedUnits)
	return out, nil
}

// pendingHookNames returns the hook names from the pending map in sorted order.
// Sorted output keeps log lines and tests deterministic.
func pendingHookNames(pending map[string]int) []string {
	if len(pending) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(pending))
}

// enforceRetryBudget removes hooks that exceeded their configured max_retries.
// Hooks not found in the config are left untouched (they'll be dropped as stale
// on the next actual retry attempt by RunPendingHooks).
func enforceRetryBudget(pending map[string]int, hooks []config.Hook) map[string]int {
	if len(pending) == 0 {
		return nil
	}
	maxByName := make(map[string]int, len(hooks))
	for _, h := range hooks {
		maxByName[h.Name] = h.MaxRetries
	}
	for name, count := range pending {
		limit, ok := maxByName[name]
		if !ok {
			continue // stale hook name; RunPendingHooks will handle it
		}
		if limit <= 0 {
			limit = config.DefaultMaxRetries
		}
		if count >= limit {
			slog.Error("hook exhausted retry budget, giving up",
				"hook", name, "attempts", count, "max_retries", limit)
			delete(pending, name)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return pending
}

func (a *Agent) applyWithRollback(ctx context.Context, headSHA string, changeset *reconciler.Changeset, hooks []config.Hook, pendingNames []string) (*applier.ApplyResult, error) {
	snap, err := rollback.CreateSnapshot(changeset, os.ReadFile)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	app := applier.New(a.systemd, a.podman, a.writer, a.dryRun, hooks)
	result, err := app.ApplyWithPending(ctx, changeset, pendingNames)
	if err != nil {
		slog.Error("apply failed, rolling back", "error", err)
		metrics.RollbackTotal.Inc()

		// Use a detached context so rollback can complete even during shutdown.
		// WithoutCancel preserves parent values (e.g. trace IDs) without inheriting cancellation.
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rollbackCancel()

		if rbErr := rollback.Restore(rollbackCtx, snap, a.writer, a.systemd); rbErr != nil {
			slog.Error("rollback failed", "error", rbErr)
		} else {
			slog.Warn("rollback complete", "sha", headSHA)
		}
		return nil, fmt.Errorf("%w: apply: %w", errRollbackPerformed, err)
	}

	for _, change := range changeset.Changes {
		if change.Action != reconciler.ActionNoop {
			metrics.FilesAppliedTotal.WithLabelValues(string(change.Action), change.Category.String()).Inc()
		}
		// Remove status for units leaving management. The metrics collector
		// reads from the status store, so a single delete is sufficient.
		if change.Action == reconciler.ActionDelete && change.ServiceName != "" {
			a.statusStore.DeleteUnit(change.ServiceName)
		}
	}
	if incompleteErr := applyIncompleteError(result, changesetUnitNames(changeset)); incompleteErr != nil {
		logNonRetryableApplyErrors(result)
		// tick() logs the user-facing "apply incomplete" message with sha + duration.
		// Failed restarts of managed units are recorded in state.PendingUnits and
		// retried by health-enforce; they make the apply incomplete but do not roll
		// back (the files on disk are correct — only runtime convergence is pending).
		return result, incompleteErr
	}

	slog.Info("apply complete",
		"applied", result.Applied,
		"restarted", result.RestartedUnits,
		"dry_run", a.dryRun,
	)

	return result, nil
}

// changesetUnitNames returns the set of systemd service names the changeset
// manages. It mirrors the ServiceNames map UpdateState rebuilds, so the
// managed-unit check in applyIncompleteError agrees with prunePendingUnits on
// which units count as managed (and are therefore retryable by health-enforce).
func changesetUnitNames(changeset *reconciler.Changeset) map[string]struct{} {
	names := make(map[string]struct{})
	for _, change := range changeset.Changes {
		if change.Action != reconciler.ActionDelete && change.ServiceName != "" {
			names[change.ServiceName] = struct{}{}
		}
	}
	return names
}

// applyIncompleteError returns an ErrApplyIncomplete-wrapped error when the
// apply did not fully converge — keep_running hooks failed, or a restart of a
// picolet-managed unit failed — and nil otherwise. Only managed units (those
// in the changeset's ServiceNames) gate this: health-enforce retries them via
// state.PendingUnits, so an incomplete apply has a convergence path. A failed
// restart of a hook-only unit — one not managed as a quadlet, e.g. a host
// service whose config picolet writes via the file category — has no retry
// path, so it stays a logged non-retryable error (result.Errors) rather than
// wedging the apply incomplete forever. Failed managed restarts are surfaced as
// a single joined error; per-unit detail lives in state.PendingUnits.
func applyIncompleteError(result *applier.ApplyResult, managed map[string]struct{}) error {
	failedManaged := make([]string, 0, len(result.FailedRestartUnits))
	for _, unit := range result.FailedRestartUnits {
		if _, ok := managed[unit]; ok {
			failedManaged = append(failedManaged, unit)
		}
	}
	if len(result.RetryableErrors) == 0 && len(failedManaged) == 0 {
		return nil
	}
	incompleteErrs := slices.Clone(result.RetryableErrors)
	if len(failedManaged) > 0 {
		// FailedRestartUnits comes from a map iteration in restartUnits; sort so
		// the error string (logged and recorded as an event) is stable.
		slices.Sort(failedManaged)
		incompleteErrs = append(incompleteErrs, fmt.Errorf("units failed to restart: %v", failedManaged))
	}
	return fmt.Errorf("%w: %w", applier.ErrApplyIncomplete, errors.Join(incompleteErrs...))
}

func logNonRetryableApplyErrors(result *applier.ApplyResult) {
	if len(result.Errors) == 0 {
		return
	}
	// runHooks appends the same error pointer to both Errors and
	// RetryableErrors, so pointer-identity lookup is sufficient and avoids the
	// quadratic errors.Is walk per error.
	retryable := make(map[error]struct{}, len(result.RetryableErrors))
	for _, e := range result.RetryableErrors {
		retryable[e] = struct{}{}
	}
	for _, err := range result.Errors {
		if _, ok := retryable[err]; ok {
			continue
		}
		if fallback, ok := errors.AsType[*applier.HookFallbackError](err); ok {
			slog.Warn("hook failed, fallback restart scheduled", "unit", fallback.Unit, "error", fallback.Err)
			continue
		}
		slog.Warn("non-fatal apply error", "error", err)
	}
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

// markAppliedWithMetrics records a fully successful SHA application in both
// state and metrics.
func markAppliedWithMetrics(st *state.State, headSHA string) {
	st.MarkApplied(headSHA)
	metrics.RecordAppliedSHA(headSHA)
}

// markAppliedIncompleteWithMetrics records a SHA whose apply did not fully
// converge. The SHA is recorded so gitpoll stops reporting "Changed", but the
// last-successful timestamp (state and metric) is not advanced.
func markAppliedIncompleteWithMetrics(st *state.State, headSHA string) {
	st.MarkAppliedIncomplete(headSHA)
	metrics.RecordAppliedSHAIncomplete(headSHA)
}

// scanOrphans detects and removes files/secrets placed by a previous picolet run
// that are no longer tracked in state. Runs once at startup; errors are logged,
// not fatal. The dir overrides applied below mirror the fallback logic that
// resolver.New applies to Config.QuadletDir/SystemdDir/DataDir, so scanOrphans
// never deletes files the resolver just wrote into a test-only custom directory.
func (a *Agent) scanOrphans(ctx context.Context, store *state.Store) {
	if a.dryRun {
		return
	}
	quadletDir, systemdDir, dataDir, err := resolver.ResolveDirs(a.cfg.Rootless)
	if err != nil {
		slog.Warn("resolving dirs for orphan scan failed", "error", err)
		a.statusStore.SetOrphanScan(status.OrphanScan{Ran: true, Error: err.Error()})
		return
	}
	// cmp.Or returns the first non-empty value: a test-only override when set,
	// otherwise the production default from resolver.ResolveDirs. Keeps
	// scanOrphans pointed at exactly the directories the resolver just wrote to.
	quadletDir = cmp.Or(a.quadletDirOverride, quadletDir)
	systemdDir = cmp.Or(a.systemdDirOverride, systemdDir)
	dataDir = cmp.Or(a.dataDirOverride, dataDir)
	st, err := store.Load()
	if err != nil {
		slog.Warn("loading state for orphan scan failed", "error", err)
		a.statusStore.SetOrphanScan(status.OrphanScan{Ran: true, Error: err.Error()})
		return
	}
	scanner := orphan.New(a.writer, a.podman, quadletDir, systemdDir, dataDir)
	result, err := scanner.Scan(ctx, st.ManagedFiles)
	if err != nil {
		slog.Warn("orphan scan error", "error", err)
	}
	scan := status.OrphanScan{
		Ran:            true,
		FilesRemoved:   result.FilesRemoved,
		SecretsRemoved: result.SecretsRemoved,
	}
	if err != nil {
		scan.Error = err.Error()
	}
	a.statusStore.SetOrphanScan(scan)
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

func (a *Agent) refreshResolvedSnapshot(ctx context.Context) error {
	resolved, err := a.loadAndResolve(ctx)
	if err != nil {
		return err
	}
	a.recordHostMetadata(resolved.Host)
	deps, err := validator.AnalyzeFiles(resolved.Files, a.cfg.Rootless)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	a.statusStore.SetDependencies(deps)
	return nil
}

// recordEvent appends a state-changing event to the in-memory ring rendered
// by the dashboard. Steady-state heartbeats (noop ticks, snapshot refreshes)
// are NOT recorded — they are conveyed by LastSuccessfulReconciliationAt and
// Snapshot.VerifiedAt. The ring is reserved for events an operator would
// want to see in a panel: success, failure, paused, git_error,
// failed_sha_gate, status_error.
func (a *Agent) recordEvent(result, sha, message string) {
	a.statusStore.AddEvent(status.ReconcileEvent{
		At:      time.Now(),
		Result:  result,
		SHA:     sha,
		Message: message,
	})
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

// boolToFloat is a trivial 0/1 conversion used by Run() startup to seed
// FeatureInfo gauges. Kept in agent.go (not metrics.go) because it has no
// metrics-package dependency and lives next to its only caller.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
