package applier

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/validator"
)

// SystemdManager controls systemd units via D-Bus.
type SystemdManager interface {
	DaemonReload(ctx context.Context) error
	StartUnit(ctx context.Context, name string) error
	RestartUnit(ctx context.Context, name string) error
	GetUnitState(ctx context.Context, name string) (string, error)
	IsActive(ctx context.Context, name string) (bool, error)
}

// PodmanClient interacts with the Podman API.
type PodmanClient interface {
	SecretExists(ctx context.Context, name string) (bool, error)
	SecretCreate(ctx context.Context, name string, data []byte, replace bool) error
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

	// Sort changes by category order
	sorted := slices.Clone(cs.Changes)
	slices.SortFunc(sorted, func(a, b reconciler.Change) int {
		return cmp.Compare(categoryOrder[a.Category], categoryOrder[b.Category])
	})

	changedUnits := make(map[string]bool)

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

		var err error
		switch change.Action {
		case reconciler.ActionCreate, reconciler.ActionUpdate:
			err = a.applyCreateOrUpdate(ctx, change)
		case reconciler.ActionDelete:
			err = a.applyDelete(ctx, change)
		}

		if err != nil {
			return result, fmt.Errorf("applying %s (%s): %w", change.DestPath, change.Action, err)
		}

		result.Applied++

		// Track which units need restart
		if change.Category == "secret" {
			continue
		}

		unitName := validator.UnitNameFromPath(change.DestPath)
		if unitName != "" {
			changedUnits[unitName] = true
		}

		// Check for self-update
		if filepath.Base(change.DestPath) == "picolet.container" {
			result.NeedsSelfRestart = true
		}
	}

	if a.dryRun {
		return result, nil
	}

	// Daemon reload if any quadlet/systemd files changed
	if len(changedUnits) > 0 {
		slog.Info("running systemd daemon-reload")
		if err := a.systemd.DaemonReload(ctx); err != nil {
			return result, fmt.Errorf("daemon-reload: %w", err)
		}
	}

	// Restart changed units (picolet.service last)
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

	// Restart picolet last if needed
	if changedUnits["picolet.service"] {
		slog.Info("restarting picolet (self-update)")
		if err := a.systemd.RestartUnit(ctx, "picolet.service"); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("restarting picolet.service: %w", err))
		} else {
			result.RestartedUnits = append(result.RestartedUnits, "picolet.service")
		}
	}

	return result, nil
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
		// Podman secrets have no versioned delete — skip
		slog.Warn("skipping secret deletion (not supported)", "path", change.DestPath)
		return nil
	}
	return a.writer.Remove(change.DestPath)
}
