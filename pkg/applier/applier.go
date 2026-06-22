package applier

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
)

// ErrApplyIncomplete is returned by the agent's apply path when all file writes
// succeeded but one or more keep_running hook errors occurred. Callers should
// retry on the next tick without treating this as a reconciliation failure.
var ErrApplyIncomplete = errors.New("apply incomplete")

// HookFallbackError wraps a hook execution error that triggered a fallback unit
// restart (on_failure: restart). Callers can detect it via errors.As to log a
// distinct "fallback restart scheduled" message instead of treating it as a
// generic non-fatal apply error.
type HookFallbackError struct {
	Unit string
	Err  error
}

func (e *HookFallbackError) Error() string {
	return fmt.Sprintf("hook for unit %s failed (fallback restart scheduled): %v", e.Unit, e.Err)
}

func (e *HookFallbackError) Unwrap() error { return e.Err }

// SystemdManager controls systemd units via D-Bus.
type SystemdManager interface {
	Close()
	DaemonReload(ctx context.Context) error
	StartUnit(ctx context.Context, name string) error
	StopUnit(ctx context.Context, name string) error
	RestartUnit(ctx context.Context, name string) error
	// EnableUnit links a unit's [Install] symlinks (e.g. into timers.target.wants)
	// so it persists across reboots. Required for hand-written raw systemd units;
	// quadlet-generated units realize their own [Install] and must not be enabled.
	EnableUnit(ctx context.Context, name string) error
	// DisableUnit removes a unit's [Install] symlinks. Used when a raw systemd unit
	// leaves management so the enable symlink does not outlive the unit file.
	DisableUnit(ctx context.Context, name string) error
	GetUnitStatus(ctx context.Context, name string) (UnitStatus, error)
}

// UnitStatus holds the runtime status of a systemd unit from a single D-Bus call.
// Result is intentionally omitted: it is on org.freedesktop.systemd1.Service, not
// the Unit interface, and is not accessible for all unit types (.network, .volume, .kube).
// SubState provides sufficient signal: "auto-restart" vs "failed" vs "running" vs "exited".
type UnitStatus struct {
	ActiveState   string // "active", "failed", "inactive", "activating", ...
	SubState      string // "running", "exited", "auto-restart", "dead", "waiting", ...
	UnitFileState string // "enabled", "disabled", "static", "linked", "masked", ...
}

// PodmanClient interacts with the Podman API.
type PodmanClient interface {
	SecretExists(ctx context.Context, name string) (bool, error)
	SecretCreate(ctx context.Context, name string, data []byte, replace bool) error
	SecretRemove(ctx context.Context, name string) error
	// ListManagedSecrets returns the names of all Podman secrets labelled managed-by=picolet.
	ListManagedSecrets(ctx context.Context) ([]string, error)
	ContainerRemove(ctx context.Context, nameOrID string, force bool) error
	RunHealthcheck(ctx context.Context, container string) (bool, error)
	GetPodState(ctx context.Context, pod string) (string, error)
	ContainerKill(ctx context.Context, nameOrID, signal string) error
}

// FileWriter writes files atomically.
type FileWriter interface {
	WriteFile(path string, content []byte) error
	MkdirAll(path string) error
	Remove(path string) error
}

// HookOutcome records the result of a single hook execution for metrics.
// Result values are the HookResult* constants below; they are also used as
// label values on the picolet_hook_total Prometheus metric, so keep them
// stable.
type HookOutcome struct {
	Name   string
	Action string
	Result string
}

// SystemdUnitOp records the result of a single enable/disable/start/restart
// operation on a raw systemd unit. Operation and Result are used as label
// values on the picolet_systemd_unit_operations_total Prometheus metric, so
// keep them stable.
type SystemdUnitOp struct {
	Unit      string
	Operation string // SystemdOp* below
	Result    string // SystemdOpResult* below
}

const (
	SystemdOpEnable  = "enable"
	SystemdOpDisable = "disable"
	SystemdOpStart   = "start"
	SystemdOpRestart = "restart"

	SystemdOpResultSuccess = "success"
	SystemdOpResultError   = "error"
)

const (
	// HookResultSuccess: reload reached the unit and returned 2xx (HTTP) or signal delivered.
	HookResultSuccess = "success"
	// HookResultFailed: reload was attempted but errored; retryable per OnFailure.
	HookResultFailed = "failed"
	// HookResultFallbackRestart: reload errored and on_failure: restart triggered a unit restart instead.
	HookResultFallbackRestart = "fallback_restart"
	// HookResultSkipped: reload was not attempted because it was redundant (dedup peer ran)
	// or moot (unit already scheduled for restart this tick).
	HookResultSkipped = "skipped"
)

// ApplyResult contains the outcome of an apply operation.
type ApplyResult struct {
	Applied         int
	Errors          []error
	RetryableErrors []error

	NeedsSelfRestart bool
	RestartedUnits   []string

	// DeferredSelfStops names self units whose quadlet file was deleted in this
	// changeset. Their stop is deferred to a detached goroutine in restartUnits
	// (after successor units are started) so the agent is not killed mid-apply;
	// ApplyWithoutRestarts leaves them entirely to the caller.
	DeferredSelfStops []string

	// FailedRestartUnits names units whose post-apply RestartUnit call failed.
	// The agent records these in state.PendingUnits and treats the apply as
	// incomplete (retry_pending) so the SHA is not reported as a clean success.
	FailedRestartUnits []string

	// PendingHookNames holds names of keep_running hooks that errored and should
	// retry on the next tick. Populated only when RetryableErrors is non-empty.
	PendingHookNames []string

	// FallbackRestartedUnits names units whose hook failed under on_failure:restart
	// and were therefore scheduled for restart as a fallback. Used for status and
	// metrics; per-error fallback details live in HookFallbackError wrappers in
	// Errors.
	FallbackRestartedUnits []string

	// AttemptedHookNames lists names of hooks that ran in this apply (regardless
	// of outcome). Callers use this to compute the new PendingHooks list:
	// hooks pending from a previous tick whose trigger did not change now must
	// stay pending; hooks attempted and not in PendingHookNames are cleared.
	AttemptedHookNames []string

	// HookOutcomes records per-hook execution results for metrics.
	HookOutcomes []HookOutcome

	// SystemdUnitOps records per-operation enable/disable/start/restart results
	// for raw systemd units, for metrics. These operations are best-effort: a
	// failure is logged and added to Errors but does not gate apply completeness.
	SystemdUnitOps []SystemdUnitOp
}

// Option configures an Applier.
type Option func(*Applier)

// defaultSelfUnits are the conventional units a picolet agent runs under when
// deployed from its own fleet bundle: "picolet" under the user systemd
// instance, "picolet-system" under the system instance. Stopping or restarting
// one of them synchronously would kill the agent mid-apply, before state is
// saved, so those operations are deferred (see restartUnits).
var defaultSelfUnits = []string{"picolet.service", "picolet-system.service"}

// categoryOrder is the canonical apply-phase ordering.
var categoryOrder = []config.Category{
	config.CategoryNetwork,
	config.CategoryVolume,
	config.CategorySecret,
	config.CategorySystemd,
	config.CategoryManifest,
	config.CategoryFile,
	config.CategoryContainer,
	config.CategoryKube,
}

// CategoryOrder returns the canonical apply-phase ordering. The result is a
// fresh copy so callers cannot mutate the package-level slice.
func CategoryOrder() []config.Category {
	return slices.Clone(categoryOrder)
}

var categoryRankMap = func() map[config.Category]int {
	m := make(map[config.Category]int, len(categoryOrder))
	for i, c := range categoryOrder {
		m[c] = i
	}
	return m
}()

// Applier applies a changeset to the system.
type Applier struct {
	systemd   SystemdManager
	podman    PodmanClient
	writer    FileWriter
	dryRun    bool
	hooks     []config.Hook
	reloader  *HookReloader
	selfUnits map[string]struct{}
}

// New creates a new Applier. Hooks may be nil if no change-triggered actions are needed.
func New(systemd SystemdManager, podman PodmanClient, writer FileWriter, dryRun bool, hooks []config.Hook, opts ...Option) *Applier {
	a := &Applier{
		systemd:   systemd,
		podman:    podman,
		writer:    writer,
		dryRun:    dryRun,
		hooks:     hooks,
		selfUnits: unitSet(defaultSelfUnits),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.reloader == nil {
		a.reloader = NewHookReloader(systemd, podman)
	}
	return a
}

// WithHookReloader overrides hook execution, primarily for tests.
func WithHookReloader(reloader *HookReloader) Option {
	return func(a *Applier) {
		if reloader != nil {
			a.reloader = reloader
		}
	}
}

// WithSelfUnits overrides the unit names treated as the agent's own. Pass no
// names to disable self-detection — appropriate for processes that run outside
// any managed unit (bootstrap, teardown) and handle agent stops explicitly.
func WithSelfUnits(units ...string) Option {
	return func(a *Applier) {
		a.selfUnits = unitSet(units)
	}
}

func unitSet(units []string) map[string]struct{} {
	set := make(map[string]struct{}, len(units))
	for _, unit := range units {
		set[unit] = struct{}{}
	}
	return set
}

func (a *Applier) isSelfUnit(name string) bool {
	_, ok := a.selfUnits[name]
	return ok
}

// isSelfContainer reports whether destPath is the quadlet container file of a
// self unit (e.g. picolet.container -> picolet.service).
func (a *Applier) isSelfContainer(destPath string) bool {
	unit, ok := strings.CutSuffix(filepath.Base(destPath), ".container")
	if !ok {
		return false
	}
	return a.isSelfUnit(unit + ".service")
}

// systemdActivation is one post-reload operation on a raw systemd unit.
type systemdActivation struct {
	unit string
	op   string // SystemdOp* (enable/start/restart)
}

// applyPhaseResult holds the categorized change sets produced by applyPhase.
type applyPhaseResult struct {
	ChangedUnits   map[string]struct{}
	ChangedSecrets map[string]struct{}
	ChangedRels    map[config.Category]map[string]struct{} // category -> relpath set
	// SystemdActivations are enable/start/restart operations for raw
	// (CategorySystemd) units, kept off the quadlet ChangedUnits restart path and
	// applied in order after daemon-reload, before the quadlet restarts. A unit's
	// enable is appended before its start/restart so the symlink exists first.
	SystemdActivations []systemdActivation
	NeedsReload        bool
}

// Apply applies the changeset in phased order. Equivalent to ApplyWithPending
// with no pending-hook retries — used by the CLI dry-run / one-shot apply
// where there is no agent-level pending-hook bookkeeping.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	return a.ApplyWithPending(ctx, cs, nil)
}

// ApplyWithoutRestarts applies writes/deletes and daemon-reload, preserving
// pre-delete stops, but skips post-apply hooks and unit restarts. Deferred
// self stops are left entirely to the caller (combine with WithSelfUnits()
// when synchronous pre-delete stops are wanted for agent-named units too).
func (a *Applier) ApplyWithoutRestarts(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	result := &ApplyResult{}
	phase, err := a.applyPhase(ctx, sortedByCategory(cs.Changes), result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	return result, a.reloadIfNeeded(ctx, phase.NeedsReload)
}

// sortedByCategory orders changes by the canonical apply-phase category order.
func sortedByCategory(changes []reconciler.Change) []reconciler.Change {
	sorted := slices.Clone(changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryRank(x.Category), categoryRank(y.Category))
	})
	return sorted
}

// ApplyWithPending applies the changeset and additionally retries any pending
// hook names whose triggers are not in the changeset. Pending hooks share the
// per-tick dedup map with changeset-driven hooks, so a pending hook whose
// hookExecutionKey matches a hook just run is recorded as "skipped" instead
// of producing a duplicate side-effect. Stale pending names (hook removed
// from config) are marked attempted so mergePendingHooks drops them.
func (a *Applier) ApplyWithPending(ctx context.Context, cs *reconciler.Changeset, pendingNames []string) (*ApplyResult, error) {
	result := &ApplyResult{}
	phase, err := a.applyPhase(ctx, sortedByCategory(cs.Changes), result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	hookRestartUnits := a.runHooksWithPending(ctx, phase.ChangedSecrets, phase.ChangedRels, phase.ChangedUnits, pendingNames, result)
	maps.Copy(phase.ChangedUnits, hookRestartUnits)
	return result, a.restartUnits(ctx, phase, result)
}

func categoryRank(category config.Category) int {
	rank, ok := categoryRankMap[category]
	if !ok {
		return len(categoryOrder)
	}
	return rank
}

//nolint:cyclop // multiple early-continues are clearer than restructuring
func (a *Applier) applyPhase(ctx context.Context, sorted []reconciler.Change, result *ApplyResult) (*applyPhaseResult, error) {
	p := &applyPhaseResult{
		ChangedUnits:   make(map[string]struct{}),
		ChangedSecrets: make(map[string]struct{}),
		ChangedRels:    make(map[config.Category]map[string]struct{}),
	}
	for _, change := range sorted {
		if change.Action == reconciler.ActionNoop {
			continue
		}
		slog.Info("applying change",
			"path", change.DestPath,
			"action", change.Action,
			"category", change.Category,
			"dry_run", a.dryRun,
		)
		if a.dryRun {
			result.Applied++
			continue
		}
		if change.Action == reconciler.ActionDelete && change.Category != config.CategorySecret {
			a.stopUnitForDelete(ctx, change, result)
		}
		if err := a.applyChange(ctx, change); err != nil {
			return nil, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}
		result.Applied++
		if change.Category == config.CategorySecret {
			if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
				p.ChangedSecrets[reconciler.SecretNameFromPath(change.DestPath)] = struct{}{}
			}
			continue
		}
		if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
			recordChangedRel(p.ChangedRels, change.Category, change.RelPath)
		}
		// Manifest and file categories are consumed by application hooks; they
		// are not systemd unit definitions and do not require daemon-reload.
		p.NeedsReload = p.NeedsReload || !change.Category.UsesRelPath()
		if change.Action == reconciler.ActionDelete {
			// Deleted units must NOT be restarted — the unit no longer exists after
			// daemon-reload. StopUnit above already terminated the running service.
			continue
		}
		// Raw systemd units are enabled/started/restarted via dedicated phase sets
		// (see classifySystemdActivation); they must not enter the quadlet restart
		// path even though they now carry a ServiceName.
		if change.Category == config.CategorySystemd {
			a.classifySystemdActivation(change, p)
		} else if change.ServiceName != "" {
			p.ChangedUnits[change.ServiceName] = struct{}{}
		}
		if a.isSelfContainer(change.DestPath) {
			result.NeedsSelfRestart = true
		}
	}
	return p, nil
}

// passiveUnitExts are passive activator unit types: they have no running
// process, so on create they are started and on update restarted (to reload a
// changed schedule/config). Other raw systemd units (.service, .mount) are
// treated as services.
var passiveUnitExts = map[string]struct{}{
	".timer":  {},
	".socket": {},
	".target": {},
	".path":   {},
}

// IsPassiveUnit reports whether a systemd unit name is a passive activator unit
// (.timer/.socket/.target/.path). Shared by the applier (activation choice) and
// health (which never restarts passive units).
func IsPassiveUnit(unitName string) bool {
	_, ok := passiveUnitExts[filepath.Ext(unitName)]
	return ok
}

// classifySystemdActivation records the enable/start/restart operations for a raw
// systemd (CategorySystemd) create or update, keyed off the unit suffix and
// whether the unit declares an [Install] section. Quadlet-generated units are
// never routed here — systemd refuses to enable generated units.
func (a *Applier) classifySystemdActivation(change reconciler.Change, p *applyPhaseResult) {
	unit := filepath.Base(change.DestPath)
	hasInstall := systemdUnitHasInstall(unit, change.NewContent)
	if hasInstall {
		p.SystemdActivations = append(p.SystemdActivations, systemdActivation{unit, SystemdOpEnable})
	}
	switch {
	case IsPassiveUnit(unit):
		// Start on create; restart on update so a changed OnCalendar/schedule is
		// reloaded (StartUnit on an already-active timer keeps the old schedule).
		op := SystemdOpStart
		if change.Action == reconciler.ActionUpdate {
			op = SystemdOpRestart
		}
		p.SystemdActivations = append(p.SystemdActivations, systemdActivation{unit, op})
	case hasInstall:
		// Raw .service (and other non-passive units): a long-running daemon managed
		// directly. A oneshot with no [Install] is left for its timer to trigger
		// (write + daemon-reload only).
		p.SystemdActivations = append(p.SystemdActivations, systemdActivation{unit, SystemdOpRestart})
	}
}

// systemdUnitHasInstall reports whether raw systemd content declares an [Install]
// section, parsed via the podman INI parser to avoid false matches on comments or
// values (the content carries the picolet marker comment prepended).
func systemdUnitHasInstall(unitName, content string) bool {
	unit := parser.NewUnitFile()
	unit.Filename = unitName
	if err := unit.Parse(content); err != nil {
		// Unparseable content is rejected by the validator before apply; treat as
		// no [Install] so a broken unit is not enabled.
		return false
	}
	return unit.HasGroup("Install")
}

func recordChangedRel(changedRels map[config.Category]map[string]struct{}, category config.Category, relPath string) {
	if !category.UsesRelPath() || relPath == "" {
		return
	}
	// Lazy inner-map init keeps len(changedRels) == 0 a reliable guard in
	// runHooksWithPending: only categories with a recorded rel-path enter the map.
	if changedRels[category] == nil {
		changedRels[category] = make(map[string]struct{})
	}
	changedRels[category][relPath] = struct{}{}
}

// stopUnitForDelete stops (and for raw systemd, disables) the unit backing a file
// before it is removed, so systemd terminates the managed service cleanly —
// daemon-reload alone only drops the unit definition. A self unit must not be
// stopped here: that would kill the agent mid-apply, before successor units start
// and state is saved, so its stop is deferred.
func (a *Applier) stopUnitForDelete(ctx context.Context, change reconciler.Change, result *ApplyResult) {
	unitName := unitNameForDelete(change)
	if unitName == "" {
		return
	}
	if a.isSelfUnit(unitName) {
		result.DeferredSelfStops = append(result.DeferredSelfStops, unitName)
		return
	}
	if stopErr := a.systemd.StopUnit(ctx, unitName); stopErr != nil {
		slog.Warn("stopping unit before file removal", "unit", unitName, "error", stopErr)
	}
	// Raw systemd units may carry an [Install] enable symlink (e.g. into
	// timers.target.wants) that orphan cleanup does not track. Disable while the
	// file is still on disk, before applyChange removes it.
	if change.Category == config.CategorySystemd {
		a.runSystemdOp(ctx, unitName, SystemdOpDisable, result)
	}
}

// unitNameForDelete returns the systemd unit name to stop before a file is removed.
// Quadlet categories use the pre-computed ServiceName from state.
// Systemd category: the filename IS the unit name (no parse needed).
// Secrets, manifests, and files have no associated unit.
func unitNameForDelete(change reconciler.Change) string {
	switch change.Category {
	case config.CategoryContainer, config.CategoryNetwork, config.CategoryVolume, config.CategoryKube:
		return change.ServiceName // from state.ServiceNames; "" if unknown
	case config.CategorySystemd:
		return filepath.Base(change.DestPath) // e.g. "foo.service"
	default:
		return ""
	}
}

func (a *Applier) applyChange(ctx context.Context, change reconciler.Change) error {
	switch change.Action {
	case reconciler.ActionCreate, reconciler.ActionUpdate:
		return a.applyCreateOrUpdate(ctx, change)
	case reconciler.ActionDelete:
		return a.applyDelete(ctx, change)
	default:
		return nil
	}
}

func (a *Applier) restartUnits(ctx context.Context, phase *applyPhaseResult, result *ApplyResult) error {
	if len(phase.ChangedUnits) == 0 && !phase.NeedsReload && len(result.DeferredSelfStops) == 0 &&
		len(phase.SystemdActivations) == 0 {
		return nil
	}
	// Reload first: enable/start must see the new unit files in systemd's namespace.
	if err := a.reloadIfNeeded(ctx, phase.NeedsReload); err != nil {
		return err
	}
	a.activateSystemdUnits(ctx, phase, result)
	var selfRestarts []string
	for _, unit := range slices.Sorted(maps.Keys(phase.ChangedUnits)) {
		if a.isSelfUnit(unit) {
			selfRestarts = append(selfRestarts, unit)
			continue
		}
		slog.Info("restarting unit", "unit", unit)
		if err := a.systemd.RestartUnit(ctx, unit); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("restarting %s: %w", unit, err))
			result.FailedRestartUnits = append(result.FailedRestartUnits, unit)
		} else {
			result.RestartedUnits = append(result.RestartedUnits, unit)
		}
	}
	a.scheduleSelfUnitOps(selfRestarts, result) //nolint:contextcheck // self ops intentionally detach from the apply context
	return nil
}

// activateSystemdUnits runs the enable/start/restart operations for raw systemd
// units after the daemon-reload, in the order they were recorded. Each operation
// is best-effort: failures are logged, appended to result.Errors, and recorded
// for metrics, but do not gate apply completeness — health-enforce converges
// genuinely-failed services on a later tick.
func (a *Applier) activateSystemdUnits(ctx context.Context, phase *applyPhaseResult, result *ApplyResult) {
	for _, act := range phase.SystemdActivations {
		a.runSystemdOp(ctx, act.unit, act.op, result)
	}
}

// runSystemdOp invokes one systemd operation on a raw unit and records its outcome.
func (a *Applier) runSystemdOp(ctx context.Context, unit, op string, result *ApplyResult) {
	slog.Info("systemd unit operation", "unit", unit, "operation", op)
	if err := a.systemdOpFunc(op)(ctx, unit); err != nil {
		slog.Warn("systemd unit operation failed", "unit", unit, "operation", op, "error", err)
		result.Errors = append(result.Errors, fmt.Errorf("%s %s: %w", op, unit, err))
		result.SystemdUnitOps = append(result.SystemdUnitOps, SystemdUnitOp{Unit: unit, Operation: op, Result: SystemdOpResultError})
		return
	}
	result.SystemdUnitOps = append(result.SystemdUnitOps, SystemdUnitOp{Unit: unit, Operation: op, Result: SystemdOpResultSuccess})
}

// systemdOpFunc maps a SystemdOp* constant to the SystemdManager method that runs it.
func (a *Applier) systemdOpFunc(op string) func(context.Context, string) error {
	switch op {
	case SystemdOpEnable:
		return a.systemd.EnableUnit
	case SystemdOpDisable:
		return a.systemd.DisableUnit
	case SystemdOpStart:
		return a.systemd.StartUnit
	default: // SystemdOpRestart
		return a.systemd.RestartUnit
	}
}

// scheduleSelfUnitOps fires the deferred restarts/stops of the agent's own
// units after all other units were handled synchronously.
func (a *Applier) scheduleSelfUnitOps(selfRestarts []string, result *ApplyResult) {
	for _, unit := range selfRestarts {
		slog.Info("restarting picolet (self-update), state will be saved before shutdown", "unit", unit)
		result.RestartedUnits = append(result.RestartedUnits, unit)
		a.deferredSelfUnitOp(unit, a.systemd.RestartUnit)
	}
	for _, unit := range result.DeferredSelfStops {
		if slices.Contains(selfRestarts, unit) {
			continue // restart already scheduled; a stop would defeat it
		}
		slog.Info("stopping picolet (own unit deleted), state will be saved before shutdown", "unit", unit)
		a.deferredSelfUnitOp(unit, a.systemd.StopUnit)
	}
}

// deferredSelfUnitOp runs a stop/restart of one of the agent's own units in a
// detached goroutine. Fire-and-forget: Apply() returns promptly, allowing the
// caller to remove the lock and call store.Save() before SIGTERM arrives from
// systemd's stop sequence. 60s timeout covers StopTimeout=30 + Podman cleanup.
// The context is intentionally detached from the apply context.
func (a *Applier) deferredSelfUnitOp(unit string, op func(context.Context, string) error) {
	go func() {
		opCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := op(opCtx, unit); err != nil {
			// Expected: process is killed mid-D-Bus call during shutdown.
			slog.Debug("deferred self unit operation result (may be interrupted by shutdown)", "unit", unit, "error", err)
		}
	}()
}

func (a *Applier) reloadIfNeeded(ctx context.Context, needsReload bool) error {
	if !needsReload {
		return nil
	}
	slog.Info("running systemd daemon-reload")
	if err := a.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	return nil
}

// runHooksWithPending executes hooks in two passes that share a per-tick dedup
// map: the first pass walks hooks whose triggers are in the changeset; the
// second pass retries pending names that did not appear in the first. A
// pending hook whose hookExecutionKey matches a hook just run lands as
// "skipped" via runOneHook.
//
//nolint:cyclop // two-pass loop with shared dedup is clearer as one function
func (a *Applier) runHooksWithPending(
	ctx context.Context,
	changedSecrets map[string]struct{},
	changedRels map[config.Category]map[string]struct{},
	restartScheduled map[string]struct{},
	pendingNames []string,
	result *ApplyResult,
) map[string]struct{} {
	if len(changedSecrets) == 0 && len(changedRels) == 0 && len(pendingNames) == 0 {
		return nil
	}
	if len(a.hooks) == 0 && len(pendingNames) == 0 {
		return nil
	}
	restartUnits := make(map[string]struct{})
	executed := make(map[string]struct{})
	firstPass := make(map[string]struct{})
	restartSet := make(map[string]struct{}, len(restartScheduled))
	maps.Copy(restartSet, restartScheduled)

	// First pass: hooks whose trigger is in this tick's changeset.
	for _, hook := range a.hooks {
		if !hookMatchesChange(hook, changedSecrets, changedRels) {
			continue
		}
		firstPass[hook.Name] = struct{}{}
		a.runOneHook(ctx, hook, restartSet, executed, restartUnits, result)
	}

	// Second pass: pending names not already attempted in the first pass.
	if len(pendingNames) == 0 {
		return restartUnits
	}
	byName := make(map[string]config.Hook, len(a.hooks))
	for _, h := range a.hooks {
		byName[h.Name] = h
	}
	for _, name := range pendingNames {
		if _, already := firstPass[name]; already {
			continue
		}
		hook, ok := byName[name]
		if !ok {
			// Hook was removed from config since the last failure. Mark
			// attempted (no outcome) so mergePendingHooks drops it.
			slog.Info("pending hook no longer in config, dropping", "hook", name)
			result.AttemptedHookNames = append(result.AttemptedHookNames, name)
			continue
		}
		a.runOneHook(ctx, hook, restartSet, executed, restartUnits, result)
	}
	return restartUnits
}

// runOneHook dispatches a single hook through the dedup map and the shared
// dispatchHookResult bookkeeping.
func (a *Applier) runOneHook(
	ctx context.Context,
	hook config.Hook,
	restartSet map[string]struct{},
	executed map[string]struct{},
	restartUnits map[string]struct{},
	result *ApplyResult,
) {
	key := hookExecutionKey(hook)
	if _, ran := executed[key]; ran {
		dispatchHookResult(hook, false, ErrHookSkipped, restartUnits, result)
		return
	}
	executed[key] = struct{}{}
	shouldRestart, err := a.reloader.Run(ctx, hook, restartSet)
	dispatchHookResult(hook, shouldRestart, err, restartUnits, result)
	if shouldRestart {
		restartSet[hook.Unit] = struct{}{}
	}
}

// dispatchHookResult records a hook's outcome on result and updates restartUnits.
func dispatchHookResult(hook config.Hook, shouldRestart bool, err error, restartUnits map[string]struct{}, result *ApplyResult) {
	result.AttemptedHookNames = append(result.AttemptedHookNames, hook.Name)
	outcome := HookOutcome{Name: hook.Name, Action: hook.Action}
	switch {
	case err == nil:
		outcome.Result = HookResultSuccess
	case errors.Is(err, ErrHookSkipped):
		outcome.Result = HookResultSkipped
	case shouldRestart:
		result.Errors = append(result.Errors, &HookFallbackError{Unit: hook.Unit, Err: err})
		result.FallbackRestartedUnits = append(result.FallbackRestartedUnits, hook.Unit)
		outcome.Result = HookResultFallbackRestart
	default:
		// keep_running: classify as retryable. ErrUnitNotActive lands here too.
		result.Errors = append(result.Errors, err)
		result.RetryableErrors = append(result.RetryableErrors, err)
		result.PendingHookNames = append(result.PendingHookNames, hook.Name)
		outcome.Result = HookResultFailed
	}
	result.HookOutcomes = append(result.HookOutcomes, outcome)
	if shouldRestart {
		restartUnits[hook.Unit] = struct{}{}
	}
}

// RunPendingHooks re-executes the named hooks without a changeset. Used on
// ticks where the diff is otherwise empty but pending hooks remain.
func (a *Applier) RunPendingHooks(ctx context.Context, pendingNames []string) *ApplyResult {
	result := &ApplyResult{}
	if len(pendingNames) == 0 {
		return result
	}
	restartUnits := a.runHooksWithPending(ctx, nil, nil, nil, pendingNames, result)
	if !a.dryRun {
		a.restartFallbackUnits(ctx, restartUnits, result)
	}
	return result
}

func (a *Applier) restartFallbackUnits(ctx context.Context, restartUnits map[string]struct{}, result *ApplyResult) {
	for unit := range restartUnits {
		slog.Info("restarting unit after hook retry", "unit", unit)
		if err := a.systemd.RestartUnit(ctx, unit); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("restarting %s: %w", unit, err))
		} else {
			result.RestartedUnits = append(result.RestartedUnits, unit)
		}
	}
}

// hookExecutionKey produces a deduplication key for a hook. Hooks with
// identical keys represent the same side-effect (e.g., same HTTP reload
// endpoint) and only need to run once per tick. The \x00 separator is safe
// because none of the fields (URLs, unit names, signals) can contain null bytes.
func hookExecutionKey(hook config.Hook) string {
	switch hook.Action {
	case config.HookActionHTTP:
		return hook.Action + "\x00" + hook.Unit + "\x00" + hook.Method + "\x00" + hook.URL + "\x00" + hook.HealthURL
	case config.HookActionSignal:
		return hook.Action + "\x00" + hook.Unit + "\x00" + hook.Container + "\x00" + hook.Signal
	case config.HookActionRestart:
		return hook.Action + "\x00" + hook.Unit
	default:
		return hook.Name
	}
}

func hookMatchesChange(hook config.Hook, changedSecrets map[string]struct{}, changedRels map[config.Category]map[string]struct{}) bool {
	for _, secret := range hook.Secrets {
		if _, ok := changedSecrets[secret]; ok {
			return true
		}
	}
	changedManifests := changedRels[config.CategoryManifest]
	for _, manifest := range hook.Manifests {
		if _, ok := changedManifests[manifest]; ok {
			return true
		}
	}
	changedFiles := changedRels[config.CategoryFile]
	for _, file := range hook.Files {
		if _, ok := changedFiles[file]; ok {
			return true
		}
	}
	return false
}

func (a *Applier) applyCreateOrUpdate(ctx context.Context, change reconciler.Change) error {
	if change.Category == config.CategorySecret {
		name := reconciler.SecretNameFromPath(change.DestPath)
		replace := change.Action == reconciler.ActionUpdate
		return a.podman.SecretCreate(ctx, name, []byte(change.NewContent), replace)
	}

	// Regular file: ensure directory exists, write atomically.
	dir := filepath.Dir(change.DestPath)
	if err := a.writer.MkdirAll(dir); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return a.writer.WriteFile(change.DestPath, []byte(change.NewContent))
}

func (a *Applier) applyDelete(ctx context.Context, change reconciler.Change) error {
	if change.Category == config.CategorySecret {
		name := reconciler.SecretNameFromPath(change.DestPath)
		return a.podman.SecretRemove(ctx, name)
	}
	return a.writer.Remove(change.DestPath)
}
