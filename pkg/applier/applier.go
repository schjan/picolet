package applier

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// of outcome). Callers use this to compute the new PendingSecretHooks list:
	// hooks pending from a previous tick whose secret did not change now must
	// stay pending; hooks attempted and not in PendingHookNames are cleared.
	AttemptedHookNames []string
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
	systemd     SystemdManager
	podman      PodmanClient
	writer      FileWriter
	dryRun      bool
	secretHooks []config.SecretHook
	reloader    *SecretHookReloader
}

// New creates a new Applier.
func New(systemd SystemdManager, podman PodmanClient, writer FileWriter, dryRun bool, opts ...Option) *Applier {
	a := &Applier{
		systemd: systemd,
		podman:  podman,
		writer:  writer,
		dryRun:  dryRun,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.reloader == nil {
		a.reloader = NewSecretHookReloader(systemd, podman)
	}
	return a
}

// WithSecretHooks configures hooks to execute after matching secrets change.
func WithSecretHooks(hooks []config.SecretHook) Option {
	return func(a *Applier) {
		a.secretHooks = slices.Clone(hooks)
	}
}

// WithSecretHookReloader overrides hook execution, primarily for tests.
func WithSecretHookReloader(reloader *SecretHookReloader) Option {
	return func(a *Applier) {
		if reloader != nil {
			a.reloader = reloader
		}
	}
}

// Apply applies the changeset in phased order.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	result := &ApplyResult{}
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryRank(x.Category), categoryRank(y.Category))
	})

	changedUnits, changedSecrets, needsReload, err := a.applyPhase(ctx, sorted, result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	hookRestartUnits := a.runSecretHooks(ctx, changedSecrets, changedUnits, result)
	mergeUnitSet(changedUnits, hookRestartUnits)
	return result, a.restartUnits(ctx, changedUnits, needsReload, result)
}

func categoryRank(category string) int {
	rank, ok := categoryRankMap[category]
	if !ok {
		return categoryRankMap["unknown"]
	}
	return rank
}

//nolint:cyclop // multiple early-continues are clearer than restructuring
func (a *Applier) applyPhase(ctx context.Context, sorted []reconciler.Change, result *ApplyResult) (changedUnits map[string]bool, changedSecrets map[string]bool, needsReload bool, err error) {
	changedUnits = make(map[string]bool)
	changedSecrets = make(map[string]bool)
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
			return nil, nil, false, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}
		result.Applied++
		if change.Category == "secret" {
			if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
				changedSecrets[reconciler.SecretNameFromPath(change.DestPath)] = true
			}
			continue
		}
		// All non-secret file changes (including deletes) require a daemon-reload.
		needsReload = true
		if change.Action == reconciler.ActionDelete {
			// Deleted units must NOT be restarted — the unit no longer exists after
			// daemon-reload. StopUnit above already terminated the running service.
			continue
		}
		if change.ServiceName != "" {
			changedUnits[change.ServiceName] = true
		}
		if filepath.Base(change.DestPath) == selfContainerFile {
			result.NeedsSelfRestart = true
		}
	}
	return changedUnits, changedSecrets, needsReload, nil
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

func (a *Applier) restartUnits(ctx context.Context, changedUnits map[string]bool, needsReload bool, result *ApplyResult) error {
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
	if changedUnits["picolet.service"] {
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

func (a *Applier) runSecretHooks(ctx context.Context, changedSecrets map[string]bool, restartScheduled map[string]bool, result *ApplyResult) map[string]bool {
	if len(changedSecrets) == 0 || len(a.secretHooks) == 0 {
		return nil
	}
	restartUnits := make(map[string]bool)
	executed := make(map[string]bool)
	for _, hook := range a.secretHooks {
		if !hookMatchesChangedSecret(hook, changedSecrets) {
			continue
		}
		key := hookExecutionKey(hook)
		if executed[key] {
			// Another hook with the same dedup key already ran this tick —
			// its outcome covers this hook too. Mark attempted so a stale
			// pending entry for this hook gets cleared by mergePendingHooks.
			result.AttemptedHookNames = append(result.AttemptedHookNames, hook.Name)
			continue
		}
		executed[key] = true
		restartSet := make(map[string]bool, len(restartScheduled)+len(restartUnits))
		mergeUnitSet(restartSet, restartScheduled)
		mergeUnitSet(restartSet, restartUnits)
		shouldRestart, err := a.reloader.Run(ctx, hook, restartSet)
		dispatchHookResult(hook, shouldRestart, err, restartUnits, result)
	}
	return restartUnits
}

// dispatchHookResult records a hook's outcome on result and updates restartUnits.
// Shared by runSecretHooks and RunPendingHooks so error classification, attempt
// tracking, and restart-set updates stay consistent across both paths.
func dispatchHookResult(hook config.SecretHook, shouldRestart bool, err error, restartUnits map[string]bool, result *ApplyResult) {
	result.AttemptedHookNames = append(result.AttemptedHookNames, hook.Name)
	if err != nil {
		if shouldRestart {
			result.Errors = append(result.Errors, &HookFallbackError{Unit: hook.Unit, Err: err})
			result.FallbackRestartedUnits = append(result.FallbackRestartedUnits, hook.Unit)
		} else {
			result.Errors = append(result.Errors, err)
			result.RetryableErrors = append(result.RetryableErrors, err)
			result.PendingHookNames = append(result.PendingHookNames, hook.Name)
		}
	}
	if shouldRestart {
		restartUnits[hook.Unit] = true
	}
}

// RunPendingHooks re-executes hooks named in pendingNames without a changeset.
// Hook names not present in the configured hooks are silently dropped (the
// hook was removed from config since the last failure). Hooks that fall back
// to restart cause the affected unit to be restarted inline (no daemon-reload —
// the file system already matches state, only the unit config changed).
func (a *Applier) RunPendingHooks(ctx context.Context, pendingNames []string) *ApplyResult {
	result := &ApplyResult{}
	if len(pendingNames) == 0 || len(a.secretHooks) == 0 {
		return result
	}
	byName := make(map[string]config.SecretHook, len(a.secretHooks))
	for _, h := range a.secretHooks {
		byName[h.Name] = h
	}

	restartUnits := make(map[string]bool)
	for _, name := range pendingNames {
		hook, ok := byName[name]
		if !ok {
			// Mark stale names as attempted so mergePendingHooks drops them
			// instead of carrying them forward forever.
			slog.Info("pending secret hook no longer in config, dropping", "hook", name)
			result.AttemptedHookNames = append(result.AttemptedHookNames, name)
			continue
		}
		shouldRestart, err := a.reloader.Run(ctx, hook, restartUnits)
		dispatchHookResult(hook, shouldRestart, err, restartUnits, result)
	}

	if !a.dryRun {
		a.restartFallbackUnits(ctx, restartUnits, result)
	}
	return result
}

func (a *Applier) restartFallbackUnits(ctx context.Context, restartUnits map[string]bool, result *ApplyResult) {
	for unit := range restartUnits {
		slog.Info("restarting unit after secret hook retry", "unit", unit)
		if err := a.systemd.RestartUnit(ctx, unit); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("restarting %s: %w", unit, err))
		} else {
			result.RestartedUnits = append(result.RestartedUnits, unit)
		}
	}
}

func hookExecutionKey(hook config.SecretHook) string {
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

func hookMatchesChangedSecret(hook config.SecretHook, changedSecrets map[string]bool) bool {
	for _, secret := range hook.Secrets {
		if changedSecrets[secret] {
			return true
		}
	}
	return false
}

func mergeUnitSet(dst, src map[string]bool) {
	for k, v := range src {
		if v {
			dst[k] = true
		}
	}
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
