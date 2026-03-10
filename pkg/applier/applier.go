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
)

// volumePhaseMax is the highest categoryOrder value belonging to the pre-phase
// (network + volume). Changes at or below this value are applied before the
// configfile phase so that volume units can be daemon-reloaded and started.
const volumePhaseMax = 1 // categoryOrder["volume"]

// SystemdManager controls systemd units via D-Bus.
type SystemdManager interface {
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
	// VolumeInspectMountpoint returns the host filesystem mountpoint for the named volume.
	VolumeInspectMountpoint(ctx context.Context, name string) (string, error)
	// VolumeRemove removes a named Podman volume. It is a no-op if the volume does not exist.
	VolumeRemove(ctx context.Context, name string) error
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

// selfContainerFile is the quadlet filename for picolet's own container unit.
// Its presence in a create/update changeset triggers a self-restart via picolet.service.
const selfContainerFile = "picolet.container"

// categoryOrder defines the apply phase ordering.
var categoryOrder = map[string]int{
	"network":    0,
	"volume":     1,
	"configfile": 2,
	"secret":     3,
	"systemd":    4,
	"manifest":   5,
	"container":  6,
	"kube":       7,
	"unknown":    8,
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
//
// Changes are split into a pre-phase (network + volume) and a main phase
// (configfile onward). When configfile changes are present alongside volume
// changes, a DaemonReload + StartUnit is performed after the pre-phase so that
// the Podman volume exists and VolumeInspectMountpoint succeeds.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	result := &ApplyResult{}
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryOrder[x.Category], categoryOrder[y.Category])
	})

	split := splitPhase(sorted)
	prePhase, mainPhase := sorted[:split], sorted[split:]

	preChangedUnits, preNeedsReload, err := a.applyPhase(ctx, prePhase, result)
	if err != nil {
		return result, err
	}

	if !a.dryRun {
		// Bootstrap volume units only when configfiles are also being applied in
		// this changeset. This avoids an extra DaemonReload when no configfiles exist.
		hasConfigfiles := slices.ContainsFunc(mainPhase, func(c reconciler.Change) bool {
			return c.Category == "configfile" && c.Action != reconciler.ActionNoop
		})
		if hasConfigfiles {
			if err := a.maybeBootstrapVolumes(ctx, prePhase, preNeedsReload); err != nil {
				return result, err
			}
		}
	}

	mainChangedUnits, mainNeedsReload, err := a.applyPhase(ctx, mainPhase, result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}
	for u := range preChangedUnits {
		mainChangedUnits[u] = true
	}
	return result, a.restartUnits(ctx, mainChangedUnits, preNeedsReload || mainNeedsReload, result)
}

// splitPhase returns the index of the first change in the main phase (configfile
// and beyond). The input must already be sorted by categoryOrder.
func splitPhase(sorted []reconciler.Change) int {
	for i, c := range sorted {
		if categoryOrder[c.Category] > volumePhaseMax {
			return i
		}
	}
	return len(sorted)
}

// maybeBootstrapVolumes performs a DaemonReload and starts new/updated volume
// units when the pre-phase wrote volume files. This ensures the Podman volume
// exists and VolumeInspectMountpoint succeeds before the configfile phase runs.
//
//nolint:cyclop // sequential guard clauses + per-volume loop; extraction would obscure the flow
func (a *Applier) maybeBootstrapVolumes(ctx context.Context, prePhase []reconciler.Change, preNeedsReload bool) error {
	if !preNeedsReload {
		return nil
	}
	hasNewVolumes := slices.ContainsFunc(prePhase, func(c reconciler.Change) bool {
		return c.Category == "volume" && c.Action != reconciler.ActionDelete
	})
	if !hasNewVolumes {
		return nil
	}
	slog.Info("bootstrapping volume units before configfile phase")
	if err := a.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("daemon-reload (volume bootstrap): %w", err)
	}
	for _, c := range prePhase {
		if c.Category != "volume" || c.Action == reconciler.ActionDelete || c.ServiceName == "" {
			continue
		}
		slog.Info("starting volume unit (bootstrap)", "unit", c.ServiceName)
		if err := a.systemd.StartUnit(ctx, c.ServiceName); err != nil {
			// Log but don't fail — the volume may already be running (update case).
			slog.Warn("starting volume unit failed, continuing", "unit", c.ServiceName, "error", err)
			continue
		}
		// StartUnit is asynchronous: the Podman volume create command runs inside the
		// service and may not finish before VolumeInspectMountpoint is called.
		// Poll until active (create completed) or failed.
		if err := waitUnitActive(ctx, a.systemd, c.ServiceName); err != nil {
			return fmt.Errorf("volume unit %s not ready after start: %w", c.ServiceName, err)
		}
	}
	return nil
}

// waitUnitActive polls systemd until the unit reaches "active" state or context deadline.
// Returns an error if the unit enters "failed" state or the context is cancelled.
func waitUnitActive(ctx context.Context, sys SystemdManager, unit string) error {
	for {
		state, err := sys.GetUnitState(ctx, unit)
		if err != nil {
			return fmt.Errorf("getting state for %s: %w", unit, err)
		}
		switch state {
		case "active":
			return nil
		case "failed":
			return fmt.Errorf("volume unit %s failed", unit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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
			if unitName := unitNameForDelete(change); unitName != "" {
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
		if change.Category == "configfile" {
			// Config files are not systemd units — no daemon-reload needed.
			// If restart_service is set, schedule a restart for create, update, AND delete
			// (removing a config means the service must reload its configuration).
			if change.ServiceName != "" {
				changedUnits[change.ServiceName] = true
			}
			continue
		}
		// All non-secret, non-configfile file changes require a daemon-reload.
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
	return changedUnits, needsReload, nil
}

// unitNameForDelete returns the systemd unit name to stop before a file is removed.
// Quadlet categories use the pre-computed ServiceName from state.
// Systemd category: the filename IS the unit name (no parse needed).
// Secrets, manifests, and configfiles don't have associated units.
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

func (a *Applier) applyCreateOrUpdate(ctx context.Context, change reconciler.Change) error {
	if change.Category == "secret" {
		name := reconciler.SecretNameFromPath(change.DestPath)
		replace := change.Action == reconciler.ActionUpdate
		return a.podman.SecretCreate(ctx, name, []byte(change.NewContent), replace)
	}
	if change.Category == "configfile" {
		return a.applyConfigFile(ctx, change)
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
	if change.Category == "configfile" {
		return a.deleteConfigFile(ctx, change)
	}
	return a.writer.Remove(change.DestPath)
}

// resolveConfigFilePath parses a "volumefile:<vol>:<relpath>" dest path and resolves
// the real host filesystem path via the volume's mountpoint.
func (a *Applier) resolveConfigFilePath(ctx context.Context, destPath string) (volName, relPath, fullPath string, err error) {
	volName, relPath, ok := reconciler.VolumeFileFromPath(destPath)
	if !ok {
		return "", "", "", fmt.Errorf("malformed configfile dest path: %q", destPath)
	}
	if !filepath.IsLocal(relPath) {
		return "", "", "", fmt.Errorf("config file path %q escapes volume mountpoint", relPath)
	}
	mountpoint, err := a.podman.VolumeInspectMountpoint(ctx, volName)
	if err != nil {
		return "", "", "", fmt.Errorf("inspecting volume %s: %w", volName, err)
	}
	return volName, relPath, filepath.Join(mountpoint, relPath), nil
}

func (a *Applier) applyConfigFile(ctx context.Context, change reconciler.Change) error {
	volName, relPath, fullPath, err := a.resolveConfigFilePath(ctx, change.DestPath)
	if err != nil {
		return err
	}
	slog.Info("writing config file to volume",
		"volume", volName,
		"path", relPath,
		"action", change.Action,
	)
	if err := a.writer.MkdirAll(filepath.Dir(fullPath)); err != nil {
		return fmt.Errorf("mkdir for config file %s: %w", fullPath, err)
	}
	return a.writer.WriteFile(fullPath, []byte(change.NewContent))
}

func (a *Applier) deleteConfigFile(ctx context.Context, change reconciler.Change) error {
	volName, relPath, fullPath, err := a.resolveConfigFilePath(ctx, change.DestPath)
	if err != nil {
		return err
	}
	slog.Info("removing config file from volume",
		"volume", volName,
		"path", relPath,
	)
	return a.writer.Remove(fullPath)
}
