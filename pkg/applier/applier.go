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
	// VolumeImportFiles imports files into a named Podman volume via tar archive over the
	// Podman API socket. Requires Podman 4.x+. The volume must already exist.
	VolumeImportFiles(ctx context.Context, volumeName string, files map[string][]byte) error
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

// Apply applies the changeset in category order.
//
// All non-configfile changes are written to disk in a single phase. Configfile
// changes are collected and imported into their volumes via VolumeImportFiles
// after a DaemonReload + volume bootstrap (when needed). This avoids requiring
// host filesystem access to volume mountpoints.
func (a *Applier) Apply(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error) {
	result := &ApplyResult{}
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(x, y reconciler.Change) int {
		return cmp.Compare(categoryOrder[x.Category], categoryOrder[y.Category])
	})

	changedUnits, needsReload, configChanges, err := a.applyPhase(ctx, sorted, result)
	if err != nil {
		return result, err
	}
	if a.dryRun {
		return result, nil
	}

	// Bootstrap volume units when configfiles need importing and volume files were written.
	if len(configChanges) > 0 {
		if err := a.maybeBootstrapVolumes(ctx, sorted, needsReload); err != nil {
			return result, err
		}
		if err := a.importConfigFiles(ctx, configChanges, changedUnits); err != nil {
			return result, err
		}
	}

	return result, a.restartUnits(ctx, changedUnits, needsReload, result)
}

// maybeBootstrapVolumes performs a DaemonReload and restarts new/updated volume
// units so that VolumeImportFiles can target them. Only acts when the changeset
// contains volume writes that require a reload.
func (a *Applier) maybeBootstrapVolumes(ctx context.Context, sorted []reconciler.Change, needsReload bool) error {
	if !needsReload {
		return nil
	}
	volumeUnits := bootstrapVolumeUnits(sorted)
	if len(volumeUnits) == 0 {
		return nil
	}
	slog.Info("bootstrapping volume units before configfile import")
	if err := a.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("daemon-reload (volume bootstrap): %w", err)
	}
	for _, unit := range volumeUnits {
		slog.Info("restarting volume unit (bootstrap)", "unit", unit)
		if err := a.systemd.RestartUnit(ctx, unit); err != nil {
			return fmt.Errorf("restarting volume unit %s: %w", unit, err)
		}
	}
	return nil
}

// bootstrapVolumeUnits returns the service names of new/updated volume changes
// that need to be started before configfile import.
func bootstrapVolumeUnits(sorted []reconciler.Change) []string {
	var units []string
	for _, c := range sorted {
		if c.Category == "volume" && c.ServiceName != "" &&
			c.Action != reconciler.ActionDelete && c.Action != reconciler.ActionNoop {
			units = append(units, c.ServiceName)
		}
	}
	return units
}

// importConfigFiles groups collected configfile changes by volume name and calls
// VolumeImportFiles for each volume. Deletes are logged as warnings since stale
// files cannot be removed from volumes via the API.
func (a *Applier) importConfigFiles(ctx context.Context, changes []reconciler.Change, changedUnits map[string]bool) error {
	volFiles := make(map[string]map[string][]byte)
	for _, c := range changes {
		volName, relPath, ok := reconciler.VolumeFileFromPath(c.DestPath)
		if !ok {
			return fmt.Errorf("malformed configfile dest path: %q", c.DestPath)
		}
		if c.Action == reconciler.ActionDelete {
			slog.Warn("configfile removed from assignments but stale file remains in volume",
				"volume", volName, "path", relPath)
			if c.ServiceName != "" {
				changedUnits[c.ServiceName] = true
			}
			continue
		}
		if volFiles[volName] == nil {
			volFiles[volName] = make(map[string][]byte)
		}
		volFiles[volName][relPath] = []byte(c.NewContent)
		if c.ServiceName != "" {
			changedUnits[c.ServiceName] = true
		}
	}
	for volName, files := range volFiles {
		slog.Info("importing config files into volume", "volume", volName, "count", len(files))
		if err := a.podman.VolumeImportFiles(ctx, volName, files); err != nil {
			return fmt.Errorf("importing config files to volume %s: %w", volName, err)
		}
	}
	return nil
}

//nolint:cyclop // multiple early-continues are clearer than restructuring
func (a *Applier) applyPhase(ctx context.Context, sorted []reconciler.Change, result *ApplyResult) (
	changedUnits map[string]bool, needsReload bool, configChanges []reconciler.Change, err error,
) {
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
		// Configfile changes are collected for batch import via VolumeImportFiles
		// after all disk writes and volume bootstrap are complete.
		if change.Category == "configfile" {
			configChanges = append(configChanges, change)
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
			return nil, false, nil, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}
		result.Applied++
		if change.Category == "secret" {
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
	return changedUnits, needsReload, configChanges, nil
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
