package applier

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/validator"
)

// SystemdManager controls systemd units via D-Bus.
type SystemdManager interface {
	DaemonReload(ctx context.Context) error
	StartUnit(ctx context.Context, name string) error
	StopUnit(ctx context.Context, name string) error
	RestartUnit(ctx context.Context, name string) error
	GetUnitState(ctx context.Context, name string) (string, error)
	IsActive(ctx context.Context, name string) (bool, error)
}

// PodmanClient interacts with the Podman API.
type PodmanClient interface {
	SecretExists(ctx context.Context, name string) (bool, error)
	SecretCreate(ctx context.Context, name string, data []byte, replace bool) error
	SecretRemove(ctx context.Context, name string) error
	ContainerRemove(ctx context.Context, nameOrID string, force bool) error
	RunHealthcheck(ctx context.Context, container string) (bool, error)
	GetPodState(ctx context.Context, pod string) (string, error)
}

// FileWriter writes files atomically.
type FileWriter interface {
	WriteFile(path string, content []byte) error
	MkdirAll(path string) error
	Remove(path string) error
}

// ApplyResult contains the outcome of an apply operation.
type ApplyResult struct {
	Applied          int
	Errors           []error
	NeedsSelfRestart bool
	RestartedUnits   []string
}

// categoryOrder defines the apply phase ordering.
var categoryOrder = map[string]int{
	"network":   0,
	"volume":    1,
	"secret":    2,
	"systemd":   3,
	"manifest":  4,
	"container": 5,
	"kube":      6,
	"unknown":   7,
}

// Applier applies a changeset to the system.
type Applier struct {
	systemd SystemdManager
	podman  PodmanClient
	writer  FileWriter
	dryRun  bool
}

// New creates a new Applier.
func New(systemd SystemdManager, podman PodmanClient, writer FileWriter, dryRun bool) *Applier {
	return &Applier{
		systemd: systemd,
		podman:  podman,
		writer:  writer,
		dryRun:  dryRun,
	}
}

// Apply applies the changeset in phased order.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	result := &ApplyResult{}
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryOrder[x.Category], categoryOrder[y.Category])
	})

	changedUnits, needsReload, err := a.applyPhase(ctx, sorted, result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	return result, a.restartUnits(ctx, changedUnits, needsReload, result)
}

//nolint:cyclop // multiple early-continues are clearer than restructuring
func (a *Applier) applyPhase(ctx context.Context, sorted []reconciler.Change, result *ApplyResult) (changedUnits map[string]bool, needsReload bool, err error) {
	changedUnits = make(map[string]bool)
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
			if unitName := validator.UnitNameFromPath(change.DestPath); unitName != "" {
				if stopErr := a.systemd.StopUnit(ctx, unitName); stopErr != nil {
					slog.Warn("stopping unit before file removal", "unit", unitName, "error", stopErr)
				}
			}
		}
		if err := a.applyChange(ctx, change); err != nil {
			return nil, false, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}
		result.Applied++
		if change.Category == "secret" {
			continue
		}
		// All non-secret file changes (including deletes) require a daemon-reload.
		needsReload = true
		if change.Action == reconciler.ActionDelete {
			// Deleted units must NOT be restarted — the unit no longer exists after
			// daemon-reload. StopUnit above already terminated the running service.
			continue
		}
		if unitName := validator.UnitNameFromPath(change.DestPath); unitName != "" {
			changedUnits[unitName] = true
		}
		if filepath.Base(change.DestPath) == "picolet.container" {
			result.NeedsSelfRestart = true
		}
	}
	return changedUnits, needsReload, nil
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
	slog.Info("running systemd daemon-reload")
	if err := a.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
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
		//nolint:contextcheck // intentional detached context for self-restart
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

func (a *Applier) applyCreateOrUpdate(ctx context.Context, change reconciler.Change) error {
	if change.Category == "secret" {
		name := reconciler.SecretNameFromPath(change.DestPath)
		replace := change.Action == reconciler.ActionUpdate
		return a.podman.SecretCreate(ctx, name, []byte(change.NewContent), replace)
	}

	// Regular file: ensure directory exists, write atomically
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
