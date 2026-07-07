package rollback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

// restoreTimeout bounds the detached rollback context in ApplyWithSnapshot.
const restoreTimeout = 30 * time.Second

// Snapshot stores the original content of files before an apply.
type Snapshot struct {
	// Files maps destPath to original content. nil value means the file didn't exist.
	Files map[string][]byte
}

// DiskReader reads a file from disk. Returns content or error (os.ErrNotExist if missing).
type DiskReader func(path string) ([]byte, error)

// CreateSnapshot captures the current state of files that will be changed.
// Secrets (secret:*) are skipped since Podman secrets have no versioning.
func CreateSnapshot(cs *reconciler.Changeset, diskReader DiskReader) (*Snapshot, error) {
	if diskReader == nil {
		return nil, errors.New("rollback snapshot disk reader is nil")
	}

	snap := &Snapshot{Files: make(map[string][]byte)}

	for _, change := range cs.Changes {
		if change.Action == reconciler.ActionNoop {
			continue
		}
		// Skip secrets — no rollback for Podman secrets
		if strings.HasPrefix(change.DestPath, "secret:") {
			continue
		}

		switch change.Action {
		case reconciler.ActionCreate:
			// File doesn't exist yet — store nil
			snap.Files[change.DestPath] = nil

		case reconciler.ActionUpdate, reconciler.ActionDelete:
			data, err := diskReader(change.DestPath)
			if err != nil {
				slog.Warn("snapshot: cannot read file, storing as absent",
					"path", change.DestPath, "error", err)
				snap.Files[change.DestPath] = nil
				continue
			}
			snap.Files[change.DestPath] = data
		}
	}

	return snap, nil
}

// ApplyWithSnapshot wraps an apply in the snapshot → apply → restore-on-fatal-
// error skeleton shared by the agent's reconcile path and the CLI's one-shot
// apply. pending is passed through to ApplyWithPending (nil for a plain apply).
//
// On a fatal apply error the snapshot is restored with a detached context —
// context.WithoutCancel preserves parent values without inheriting
// cancellation, so rollback can complete even during shutdown — and the apply
// error is returned with rolledBack=true. All outcome bookkeeping beyond the
// restore itself (metrics, error wrapping, incomplete-apply classification)
// stays with the caller: the agent and the CLI diverge there deliberately.
func ApplyWithSnapshot(ctx context.Context, app *applier.Applier, cs *reconciler.Changeset, pending []string, writer applier.FileWriter, systemd applier.SystemdManager) (result *applier.ApplyResult, rolledBack bool, err error) {
	snap, err := CreateSnapshot(cs, os.ReadFile)
	if err != nil {
		return nil, false, fmt.Errorf("creating snapshot: %w", err)
	}

	result, err = app.ApplyWithPending(ctx, cs, pending)
	if err == nil {
		return result, false, nil
	}

	slog.Error("apply failed, rolling back", "error", err)
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	if rbErr := Restore(rollbackCtx, snap, writer, systemd); rbErr != nil {
		slog.Error("rollback failed", "error", rbErr)
	} else {
		slog.Warn("rollback complete")
	}
	return nil, true, err
}

// Restore reverts files to their snapshot state and runs daemon-reload.
// Collects all errors instead of aborting on first failure to maximize recovery.
func Restore(ctx context.Context, snap *Snapshot, writer applier.FileWriter, systemd applier.SystemdManager) error {
	var errs []error
	for path, content := range snap.Files {
		if content == nil {
			slog.Info("rollback: removing", "path", path)
			if err := writer.Remove(path); err != nil {
				slog.Error("rollback: remove failed", "path", path, "error", err)
				errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
			}
			continue
		}

		slog.Info("rollback: restoring", "path", path)
		if err := writer.WriteFile(path, content); err != nil {
			slog.Error("rollback: restore failed", "path", path, "error", err)
			errs = append(errs, fmt.Errorf("restoring %s: %w", path, err))
		}
	}

	slog.Info("rollback: running daemon-reload")
	if err := systemd.DaemonReload(ctx); err != nil {
		errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
	}

	return errors.Join(errs...)
}
