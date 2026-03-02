package rollback

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

// Snapshot stores the original content of files before an apply.
type Snapshot struct {
	// Files maps destPath to original content. nil value means the file didn't exist.
	Files map[string][]byte
}

// Manager creates and restores snapshots.
type Manager struct {
	writer  applier.FileWriter
	systemd applier.SystemdManager
}

// New creates a new rollback Manager.
func New(writer applier.FileWriter, systemd applier.SystemdManager) *Manager {
	return &Manager{writer: writer, systemd: systemd}
}

// DiskReader reads a file from disk. Returns content or error (os.ErrNotExist if missing).
type DiskReader func(path string) ([]byte, error)

// Create captures the current state of files that will be changed.
// Secrets (secret:*) are skipped since Podman secrets have no versioning.
func (m *Manager) Create(cs *reconciler.Changeset, diskReader DiskReader) (*Snapshot, error) {
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

// Restore reverts files to their snapshot state and runs daemon-reload.
func (m *Manager) Restore(ctx context.Context, snap *Snapshot) error {
	for path, content := range snap.Files {
		if content == nil {
			// File was created — remove it
			slog.Info("rollback: removing", "path", path)
			if err := m.writer.Remove(path); err != nil {
				slog.Error("rollback: remove failed", "path", path, "error", err)
			}
			continue
		}

		// File was modified or deleted — restore original content
		slog.Info("rollback: restoring", "path", path)
		if err := m.writer.WriteFile(path, content); err != nil {
			return fmt.Errorf("rollback: restoring %s: %w", path, err)
		}
	}

	slog.Info("rollback: running daemon-reload")
	if err := m.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("rollback: daemon-reload: %w", err)
	}

	return nil
}
