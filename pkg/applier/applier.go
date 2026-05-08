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
	"time"

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
	GetUnitStatus(ctx context.Context, name string) (UnitStatus, error)
}

// UnitStatus holds the runtime status of a systemd unit from a single D-Bus call.
// Result is intentionally omitted: it is on org.freedesktop.systemd1.Service, not
// the Unit interface, and is not accessible for all unit types (.network, .volume, .kube).
// SubState provides sufficient signal: "auto-restart" vs "failed" vs "running" vs "exited".
type UnitStatus struct {
	ActiveState string // "active", "failed", "inactive", "activating", ...
	SubState    string // "running", "exited", "auto-restart", "dead", "waiting", ...
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
}

// Option configures an Applier.
type Option func(*Applier)

// selfContainerFile is the quadlet filename for picolet's own container unit.
// Its presence in a create/update changeset triggers a self-restart via picolet.service.
const selfContainerFile = "picolet.container"

// categoryOrder is the canonical apply-phase ordering. Exposed via
// CategoryOrder() so callers cannot mutate the package-level slice.
var categoryOrder = []string{
	"network",
	"volume",
	"secret",
	"systemd",
	"manifest",
	"container",
	"kube",
}

// CategoryOrder returns a copy of the canonical apply-phase ordering. Other
// packages (e.g. pkg/dashboard) consume this so they group by the same
// sequence the applier uses without being able to mutate it.
func CategoryOrder() []string {
	return slices.Clone(categoryOrder)
}

var categoryRankMap = func() map[string]int {
	m := make(map[string]int, len(categoryOrder)+1)
	for i, c := range categoryOrder {
		m[c] = i
	}
	m["unknown"] = len(categoryOrder)
	return m
}()

// Applier applies a changeset to the system.
type Applier struct {
	systemd  SystemdManager
	podman   PodmanClient
	writer   FileWriter
	dryRun   bool
	hooks    []config.Hook
	reloader *HookReloader
}

// New creates a new Applier. Hooks may be nil if no change-triggered actions are needed.
func New(systemd SystemdManager, podman PodmanClient, writer FileWriter, dryRun bool, hooks []config.Hook, opts ...Option) *Applier {
	a := &Applier{
		systemd: systemd,
		podman:  podman,
		writer:  writer,
		dryRun:  dryRun,
		hooks:   hooks,
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

// applyPhaseResult holds the categorized change sets produced by applyPhase.
type applyPhaseResult struct {
	ChangedUnits     map[string]struct{}
	ChangedSecrets   map[string]struct{}
	ChangedManifests map[string]struct{}
	NeedsReload      bool
}

// Apply applies the changeset in phased order. Equivalent to ApplyWithPending
// with no pending-hook retries — used by the CLI dry-run / one-shot apply
// where there is no agent-level pending-hook bookkeeping.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	return a.ApplyWithPending(ctx, cs, nil)
}

// ApplyWithPending applies the changeset and additionally retries any pending
// hook names whose triggers are not in the changeset. Pending hooks share the
// per-tick dedup map with changeset-driven hooks, so a pending hook whose
// hookExecutionKey matches a hook just run is recorded as "skipped" instead
// of producing a duplicate side-effect. Stale pending names (hook removed
// from config) are marked attempted so mergePendingHooks drops them.
func (a *Applier) ApplyWithPending(ctx context.Context, cs *reconciler.Changeset, pendingNames []string) (*ApplyResult, error) {
	result := &ApplyResult{}
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryRank(x.Category), categoryRank(y.Category))
	})

	phase, err := a.applyPhase(ctx, sorted, result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	hookRestartUnits := a.runHooksWithPending(ctx, phase.ChangedSecrets, phase.ChangedManifests, phase.ChangedUnits, pendingNames, result)
	maps.Copy(phase.ChangedUnits, hookRestartUnits)
	return result, a.restartUnits(ctx, phase.ChangedUnits, phase.NeedsReload, result)
}

func categoryRank(category string) int {
	rank, ok := categoryRankMap[category]
	if !ok {
		return categoryRankMap["unknown"]
	}
	return rank
}

//nolint:cyclop // multiple early-continues are clearer than restructuring
func (a *Applier) applyPhase(ctx context.Context, sorted []reconciler.Change, result *ApplyResult) (*applyPhaseResult, error) {
	p := &applyPhaseResult{
		ChangedUnits:     make(map[string]struct{}),
		ChangedSecrets:   make(map[string]struct{}),
		ChangedManifests: make(map[string]struct{}),
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
		// For non-secret deletes: stop the unit before removing the file so that
		// systemd terminates the managed container cleanly. daemon-reload alone does
		// not stop running services — it only removes the unit definition.
		if change.Action == reconciler.ActionDelete && change.Category != "secret" {
			if unitName := unitNameForDelete(change); unitName != "" {
				if stopErr := a.systemd.StopUnit(ctx, unitName); stopErr != nil {
					slog.Warn("stopping unit before file removal", "unit", unitName, "error", stopErr)
				}
			}
		}
		if err := a.applyChange(ctx, change); err != nil {
			return nil, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}
		result.Applied++
		if change.Category == "secret" {
			if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
				p.ChangedSecrets[reconciler.SecretNameFromPath(change.DestPath)] = struct{}{}
			}
			continue
		}
		if change.Category == "manifest" && change.RelPath != "" {
			if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
				p.ChangedManifests[change.RelPath] = struct{}{}
			}
		}
		// All non-secret file changes (including deletes) require a daemon-reload.
		p.NeedsReload = true
		if change.Action == reconciler.ActionDelete {
			// Deleted units must NOT be restarted — the unit no longer exists after
			// daemon-reload. StopUnit above already terminated the running service.
			continue
		}
		if change.ServiceName != "" {
			p.ChangedUnits[change.ServiceName] = struct{}{}
		}
		if filepath.Base(change.DestPath) == selfContainerFile {
			result.NeedsSelfRestart = true
		}
	}
	return p, nil
}

// unitNameForDelete returns the systemd unit name to stop before a file is removed.
// Quadlet categories use the pre-computed ServiceName from state.
// Systemd category: the filename IS the unit name (no parse needed).
// Secrets and manifests don't have associated units.
func unitNameForDelete(change reconciler.Change) string {
	switch change.Category {
	case "container", "network", "volume", "kube":
		return change.ServiceName // from state.ServiceNames; "" if unknown
	case "systemd":
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

func (a *Applier) restartUnits(ctx context.Context, changedUnits map[string]struct{}, needsReload bool, result *ApplyResult) error {
	if len(changedUnits) == 0 && !needsReload {
		return nil
	}
	if needsReload {
		slog.Info("running systemd daemon-reload")
		if err := a.systemd.DaemonReload(ctx); err != nil {
			return fmt.Errorf("daemon-reload: %w", err)
		}
	}
	for unit := range changedUnits {
		if unit == "picolet.service" {
			continue
		}
		slog.Info("restarting unit", "unit", unit)
		if err := a.systemd.RestartUnit(ctx, unit); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("restarting %s: %w", unit, err))
		} else {
			result.RestartedUnits = append(result.RestartedUnits, unit)
		}
	}
	if _, ok := changedUnits["picolet.service"]; ok {
		slog.Info("restarting picolet (self-update), state will be saved before shutdown")
		result.RestartedUnits = append(result.RestartedUnits, "picolet.service")
		// Fire-and-forget: detached context so Apply() returns promptly, allowing
		// applyWithRollback() to remove the lock and reconcile() to call store.Save()
		// before SIGTERM arrives from systemd's stop sequence.
		// 60s timeout covers StopTimeout=30 + Podman cleanup overhead.
		//nolint:contextcheck,gosec // intentional detached context for self-restart
		go func() {
			restartCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := a.systemd.RestartUnit(restartCtx, "picolet.service"); err != nil {
				// Expected: process is killed mid-D-Bus call during shutdown.
				slog.Debug("self-restart D-Bus result (may be interrupted by shutdown)", "error", err)
			}
		}()
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
	changedSecrets, changedManifests, restartScheduled map[string]struct{},
	pendingNames []string,
	result *ApplyResult,
) map[string]struct{} {
	if len(changedSecrets) == 0 && len(changedManifests) == 0 && len(pendingNames) == 0 {
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
		if !hookMatchesChange(hook, changedSecrets, changedManifests) {
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

func hookMatchesChange(hook config.Hook, changedSecrets, changedManifests map[string]struct{}) bool {
	for _, secret := range hook.Secrets {
		if _, ok := changedSecrets[secret]; ok {
			return true
		}
	}
	for _, manifest := range hook.Manifests {
		if _, ok := changedManifests[manifest]; ok {
			return true
		}
	}
	return false
}

func (a *Applier) applyCreateOrUpdate(ctx context.Context, change reconciler.Change) error {
	if change.Category == "secret" {
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
	if change.Category == "secret" {
		name := reconciler.SecretNameFromPath(change.DestPath)
		return a.podman.SecretRemove(ctx, name)
	}
	return a.writer.Remove(change.DestPath)
}
