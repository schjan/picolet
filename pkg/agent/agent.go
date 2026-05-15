package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
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
	pp "github.com/schjan/picolet/pkg/protonpass"
	"github.com/schjan/picolet/pkg/orphan"
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
	defaultRepoPath         = "/var/lib/picolet/repo"
	deploymentReportTimeout = 10 * time.Second

	// DefaultLockPath is the default cross-process lock for mutating commands.
	DefaultLockPath = "/var/lib/picolet/reconciliation.lock"

	// DefaultStatePath is the default location for the reconciliation state file.
	// Exported so that CLI subcommands (e.g. dry-run) can read from the same path.
	DefaultStatePath = "/var/lib/picolet/state.json"

	// healthFailureThreshold is the number of consecutive all-error health ticks
	// before /health returns 503 to trigger a container restart.
	healthFailureThreshold = 3
)

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

	opReader resolver.SecretRefReader // nil when 1Password not configured; initialized in Run
	ppReader resolver.SecretRefReader // nil when Proton Pass not configured; initialized in Run
	// Accessed only by the agent tick loop, which runs serially.
	lastOPRefresh time.Time // zero = never refreshed; in-memory only (restart always re-fetches)
	lastPPRefresh time.Time // zero = never refreshed; in-memory only (restart always re-fetches)

	webhookCh                 chan struct{}
	ready                     atomic.Bool
	paused                    atomic.Bool          // set by MQTT pause subscription
	seededSuccessfulAt        atomic.Bool          // guards one-time gauge seed from persisted state
	mqttClient                MQTTClient           // nil when MQTT not configured
	deployReporter            DeploymentReporter   // nil when GitHub App not configured
	authProvider              gitpoll.AuthProvider // nil = use default SSH/token logic
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
	}

	// Initialize Proton Pass client (if configured). EnsureSession runs inside
	// pp.NewReader; bound it with a deadline so a hung login cannot block startup.
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
	}

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
	metrics.FeatureInfo.WithLabelValues("onepassword").Set(boolToFloat(a.opReader != nil))
	metrics.FeatureInfo.WithLabelValues("protonpass").Set(boolToFloat(a.ppReader != nil))

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
		a.recordHealthMetrics(hr)
	}

	// Track consecutive D-Bus failures for /health endpoint.
	// AllFailed() is true only when every GetUnitStatus call errored — the signal
	// for D-Bus being dead (RestartUnit errors don't count because those units
	// already appeared in Unhealthy).
	if hr != nil {
		a.updateHealthFailures(hr)
	}

	// 1b. Pause check — health ran, skip reconciliation when paused via MQTT
	if a.paused.Load() {
		slog.Debug("reconciliation paused via MQTT")
		metrics.ReconciliationTotal.WithLabelValues("paused").Inc()
		a.recordEvent("paused", st.AppliedSHA, "reconciliation paused via MQTT")
		return nil
	}

	// 2. Git poll
	pollResult, err := poller.Poll(ctx, st.AppliedSHA)
	if err != nil {
		metrics.GitPollTotal.WithLabelValues("error").Inc()
		// SHA intentionally empty: the failure is about the upstream poll, not
		// about the currently-applied SHA, which would be misleading.
		a.recordEvent("git_error", "", err.Error())
		return fmt.Errorf("polling git: %w", err)
	}

	if !pollResult.Changed && !a.opRefreshDue() && !a.ppRefreshDue() && len(st.PendingHooks) == 0 {
		metrics.GitPollTotal.WithLabelValues("noop").Inc()
		slog.Debug("reconciliation: noop", "sha", pollResult.HeadSHA, "reason", "no_git_changes")
		metrics.ReconciliationTotal.WithLabelValues("noop").Inc()
		if a.statusNeedsResolvedSnapshot() {
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

	switch {
	case pollResult.Changed:
		metrics.GitPollTotal.WithLabelValues("changed").Inc()
	case len(st.PendingHooks) > 0:
		// Pending-hook retry takes priority over OP refresh in the label even
		// when both apply: the retry is the actionable reason this tick ran,
		// and ReconcileOnce will refresh op:// secrets regardless.
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
		// Still update lastOPRefresh/lastPPRefresh so the agent does not retry
		// secret refreshes every tick while blocked by the failed-SHA gate.
		if a.opReader != nil {
			a.lastOPRefresh = time.Now()
		}
		if a.ppReader != nil {
			a.lastPPRefresh = time.Now()
		}
		a.recordEvent("failed_sha_gate", pollResult.HeadSHA, "skipped after repeated failures")
		return nil
	}

	// Report deployments only for new git SHAs; forced reconciliations on the
	// same SHA (OP refresh, pending hook retry) are not deployments.
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
		if a.opReader != nil {
			a.lastOPRefresh = time.Now()
		}
		if a.ppReader != nil {
			a.lastPPRefresh = time.Now()
		}
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

	if a.opReader != nil {
		a.lastOPRefresh = time.Now()
		if result.OpSecretsCount > 0 {
			metrics.SecretSyncTotal.WithLabelValues("onepassword", "success").Inc()
			metrics.SecretLastSyncTimestamp.WithLabelValues("onepassword").SetToCurrentTime()
		}
	}
	if a.ppReader != nil {
		a.lastPPRefresh = time.Now()
		if result.PPSecretsCount > 0 {
			metrics.SecretSyncTotal.WithLabelValues("protonpass", "success").Inc()
			metrics.SecretLastSyncTimestamp.WithLabelValues("protonpass").SetToCurrentTime()
		}
	}
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
		QuadletDir: a.quadletDirOverride,
		SystemdDir: a.systemdDirOverride,
		DataDir:    a.dataDirOverride,
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
		Op: a.recordOpSecretsCount(files),
		PP: a.recordPPSecretsCount(files),
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
		a.savePartialStateOnHookFailure(headSHA, st, store, changeset, applyResult, deps, resolved.Hooks)
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
// other hosts. When the SHA is unchanged (e.g. OP refresh), it only bumps the
// verified-OK timestamp in memory.
//
// Note on validation failure: this path treats it as a hard failure and
// surfaces it through the failure-event/failed-SHA pipeline. The outer-tick
// noop fast path (no_git_changes) is more lenient: validation errors there
// during refreshResolvedSnapshot are logged and recorded as status_error
// without aborting the verify-OK signal, because nothing triggered the tick.
// Here, something *did* trigger the tick (OP refresh or git change that
// rendered identically), so a bad render is actionable.
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

// savePartialStateOnHookFailure persists state after a successful apply that
// only failed at the keep_running hook stage. Files and secrets are recorded
// (so the next tick does not re-write them), the SHA is marked applied (so
// gitpoll stops reporting "Changed" on every retry tick — which would otherwise
// produce duplicate deployment reports for the same SHA), and the pending hook
// list is saved so retry survives an agent restart.
func (a *Agent) savePartialStateOnHookFailure(headSHA string, st *state.State, store *state.Store, changeset *reconciler.Changeset, applyResult *applier.ApplyResult, deps map[string]status.UnitDependencies, hooks []config.Hook) {
	UpdateState(st, changeset)
	st.PendingHooks = enforceRetryBudget(mergePendingHooks(st.PendingHooks, applyResult), hooks)
	markAppliedWithMetrics(st, headSHA)
	a.statusStore.SetVerifiedAt(st.LastSuccessfulReconciliationAt)
	if saveErr := store.Save(st); saveErr != nil {
		slog.Error("saving partial state after hook failure", "error", saveErr)
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
			metrics.FilesAppliedTotal.WithLabelValues(string(change.Action), change.Category).Inc()
		}
		// Remove status for units leaving management. The metrics collector
		// reads from the status store, so a single delete is sufficient.
		if change.Action == reconciler.ActionDelete && change.ServiceName != "" {
			a.statusStore.DeleteUnit(change.ServiceName)
		}
	}
	if len(result.RetryableErrors) > 0 {
		logNonRetryableApplyErrors(result)
		// tick() logs the user-facing "apply incomplete" message with sha + duration.
		return result, fmt.Errorf("%w: %w", applier.ErrApplyIncomplete, errors.Join(result.RetryableErrors...))
	}

	slog.Info("apply complete",
		"applied", result.Applied,
		"restarted", result.RestartedUnits,
		"dry_run", a.dryRun,
	)

	return result, nil
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

func recordHookMetrics(result *applier.ApplyResult) {
	if result == nil {
		return
	}
	for _, o := range result.HookOutcomes {
		metrics.HookTotal.WithLabelValues(o.Name, o.Action, o.Result).Inc()
	}
}

func (a *Agent) resolvePollerAuth(ctx context.Context) (gitpoll.AuthProvider, error) {
	if a.authProvider != nil {
		return a.authProvider, nil
	}

	// Mutual exclusion is enforced by agentcfg.Validate, so at most one
	// provider's git_token_ref is set; the file-based path is also excluded.
	// NOTE: the git token is resolved once at startup. If the underlying
	// secret rotates, a picolet restart is required — the per-tick refresh
	// cycle re-fetches assignment secrets but does NOT refresh the git token.
	if a.opReader != nil && a.cfg.OnePassword != nil && a.cfg.OnePassword.GitTokenRef != "" {
		token, err := resolveGitToken(ctx, "1password", a.opReader, a.cfg.OnePassword.GitTokenRef)
		if err != nil {
			return nil, err
		}
		return gitpoll.NewStaticTokenAuth(a.cfg.RepoURL, token), nil
	}
	if a.ppReader != nil && a.cfg.ProtonPass != nil && a.cfg.ProtonPass.GitTokenRef != "" {
		token, err := resolveGitToken(ctx, "protonpass", a.ppReader, a.cfg.ProtonPass.GitTokenRef)
		if err != nil {
			return nil, err
		}
		return gitpoll.NewStaticTokenAuth(a.cfg.RepoURL, token), nil
	}

	if gitpoll.IsSSHURL(a.cfg.RepoURL) {
		return gitpoll.NewSSHAgentAuth(a.cfg.RepoURL), nil
	}
	return gitpoll.NewTokenFileAuth(a.cfg.GitTokenPath), nil
}

// resolveGitToken fetches a single ref via the given provider reader and
// validates that the response actually contains it.
func resolveGitToken(ctx context.Context, provider string, reader resolver.SecretRefReader, ref string) (string, error) {
	results, err := reader(ctx, []string{ref})
	if err != nil {
		return "", fmt.Errorf("resolving git token from %s: %w", provider, err)
	}
	token, ok := results[ref]
	if !ok {
		return "", fmt.Errorf("resolving git token from %s: ref %q not in response", provider, ref)
	}
	slog.Info("git token resolved", "provider", provider)
	return token, nil
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

func (a *Agent) updateHealthFailures(hr *health.CheckResult) {
	if hr.AllFailed() {
		a.consecutiveHealthFailures.Add(1)
	} else {
		a.consecutiveHealthFailures.Store(0)
	}
}

func (a *Agent) recordHealthMetrics(hr *health.CheckResult) {
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
	// Status store is the single source of truth for per-unit health.
	// metrics.NewUnitHealthCollector reads its scrape data from the store.
	a.statusStore.SetUnits(unitStatusesFromHealth(hr.Statuses))

	metrics.HealthCheckErrorsTotal.Add(float64(len(hr.Errors)))

	if hr.AllFailed() {
		a.statusStore.ClearUnits()
	}
}

func unitStatusesFromHealth(statuses map[string]applier.UnitStatus) map[string]status.UnitRuntimeStatus {
	out := make(map[string]status.UnitRuntimeStatus, len(statuses))
	for unit, st := range statuses {
		out[unit] = status.UnitRuntimeStatus{ActiveState: st.ActiveState, SubState: st.SubState}
	}
	return out
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
// applyDirOverrides applies the agent's WithQuadletDir/WithSystemdDir/WithDataDir
// overrides on top of already-resolved directory defaults. Mirrors the same
// logic that resolver.New applies to Config.QuadletDir/SystemdDir/DataDir;
// scanOrphans must stay in sync with that path so it never deletes files the
// resolver just wrote into a custom directory.
func (a *Agent) applyDirOverrides(quadletDir, systemdDir, dataDir string) (string, string, string) {
	if a.quadletDirOverride != "" {
		quadletDir = a.quadletDirOverride
	}
	if a.systemdDirOverride != "" {
		systemdDir = a.systemdDirOverride
	}
	if a.dataDirOverride != "" {
		dataDir = a.dataDirOverride
	}
	return quadletDir, systemdDir, dataDir
}

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
	quadletDir, systemdDir, dataDir = a.applyDirOverrides(quadletDir, systemdDir, dataDir)
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

// recordOpSecretsCount updates the per-provider secrets-managed gauge for
// 1Password and returns the count.
func (a *Agent) recordOpSecretsCount(files []resolver.ResolvedFile) int {
	if a.opReader == nil {
		return 0
	}
	count := countRefs(files, op.IsRef)
	metrics.SecretsManagedCount.WithLabelValues("onepassword").Set(float64(count))
	return count
}

// recordPPSecretsCount updates the per-provider secrets-managed gauge for
// Proton Pass and returns the count.
func (a *Agent) recordPPSecretsCount(files []resolver.ResolvedFile) int {
	if a.ppReader == nil {
		return 0
	}
	count := countRefs(files, pp.IsRef)
	metrics.SecretsManagedCount.WithLabelValues("protonpass").Set(float64(count))
	return count
}

func countRefs(files []resolver.ResolvedFile, isRef func(string) bool) int {
	var count int
	for _, f := range files {
		if isRef(f.SrcPath) {
			count++
		}
	}
	return count
}

func (a *Agent) recordHostMetadata(host *config.HostConfig) {
	if host == nil {
		return
	}
	a.statusStore.SetHost(status.HostMetadata{
		PiType:           host.PiType,
		Features:         host.Features,
		ExternalHostname: host.ExternalHostname,
	})
}

func (a *Agent) statusNeedsResolvedSnapshot() bool {
	return !a.statusStore.Snapshot().Bootstrapped
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

// opRefreshDue reports whether op:// secrets should be re-fetched.
// Returns true when 1Password is configured and the refresh interval has elapsed.
// opReader is non-nil iff cfg.OnePassword is non-nil, so a single nil check suffices.
func (a *Agent) opRefreshDue() bool {
	if a.opReader == nil {
		return false
	}
	interval := a.cfg.OnePassword.RefreshInterval
	return a.lastOPRefresh.IsZero() || time.Since(a.lastOPRefresh) >= interval
}

// ppRefreshDue reports whether pass:// secrets should be re-fetched.
// Returns true when Proton Pass is configured and the refresh interval has elapsed.
// ppReader is non-nil iff cfg.ProtonPass is non-nil, so a single nil check suffices.
func (a *Agent) ppRefreshDue() bool {
	if a.ppReader == nil {
		return false
	}
	interval := a.cfg.ProtonPass.RefreshInterval
	return a.lastPPRefresh.IsZero() || time.Since(a.lastPPRefresh) >= interval
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
		if !a.paused.Load() && a.consecutiveHealthFailures.Load() >= healthFailureThreshold {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"systemd_unreachable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/webhook", webhookHandler(a.triggerReconcile, a.cfg.WebhookSecretPath))
	if a.routeRegistrar != nil {
		a.routeRegistrar.Register(mux)
	}
	return mux
}

func (a *Agent) startHTTP() (func(context.Context), error) {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.MetricsPort),
		Handler:           a.newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("starting http listener on %s: %w", srv.Addr, err)
	}

	slog.Info("http server starting", "port", a.cfg.MetricsPort)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http server shutdown error", "error", err)
		}
	}, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// createDeployment creates a GitHub deployment and reports in_progress if a reporter is configured.
// Returns 0 when no reporter is set or when deployment creation itself fails.
func (a *Agent) createDeployment(ctx context.Context, sha string) int64 {
	if a.deployReporter == nil {
		return 0
	}
	deploymentID, err := a.deployReporter.CreateDeployment(ctx, sha)
	if err == nil {
		metrics.DeploymentStatusTotal.WithLabelValues("pending").Inc()
	}
	if err != nil {
		slog.Warn("deployment status: create failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		if deploymentID == 0 {
			return 0
		}
		slog.Info("deployment status: continuing with created deployment despite pending status error", "deployment_id", deploymentID)
	}

	if err := a.deployReporter.ReportInProgress(ctx, deploymentID); err != nil {
		slog.Warn("deployment status: in_progress failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
	} else {
		metrics.DeploymentStatusTotal.WithLabelValues("in_progress").Inc()
	}
	return deploymentID
}

// reportDeploymentResult reports the final deployment status (success/failure) if a deployment was created.
func (a *Agent) reportDeploymentResult(ctx context.Context, deploymentID int64, reconcileErr error) {
	if deploymentID == 0 || a.deployReporter == nil {
		return
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentReportTimeout)
	defer cancel()

	if reconcileErr == nil {
		a.reportDeploymentSuccess(reportCtx, deploymentID)
		return
	}
	if shouldReportDeploymentError(reconcileErr) {
		a.reportDeploymentError(reportCtx, deploymentID, reconcileErr)
		return
	}
	a.reportDeploymentFailure(reportCtx, deploymentID, reconcileErr)
}

func (a *Agent) reportDeploymentSuccess(ctx context.Context, deploymentID int64) {
	if err := a.deployReporter.ReportSuccess(ctx, deploymentID); err != nil {
		slog.Warn("deployment status: success report failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		return
	}
	metrics.DeploymentStatusTotal.WithLabelValues("success").Inc()
}

func (a *Agent) reportDeploymentFailure(ctx context.Context, deploymentID int64, reconcileErr error) {
	if err := a.deployReporter.ReportFailure(ctx, deploymentID, reconcileErr); err != nil {
		slog.Warn("deployment status: failure report failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		return
	}
	metrics.DeploymentStatusTotal.WithLabelValues("failure").Inc()
}

func (a *Agent) reportDeploymentError(ctx context.Context, deploymentID int64, reconcileErr error) {
	if err := a.deployReporter.ReportError(ctx, deploymentID, reconcileErr); err != nil {
		slog.Warn("deployment status: error report failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		return
	}
	metrics.DeploymentStatusTotal.WithLabelValues("error").Inc()
}

func shouldReportDeploymentError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(err, errRollbackPerformed)
}
