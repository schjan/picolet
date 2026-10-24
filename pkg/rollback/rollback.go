package rollback

import (
	"context"
	"errors"
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

// DiskReader reads a file from disk. Returns content or error (os.ErrNotExist if missing).
type DiskReader func(path string) ([]byte, error)

// Option configures CreateSnapshot and Restore.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// WithLogger sets the logger snapshot and restore log through. A nil logger
// is ignored; the default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

func loggerFrom(opts []Option) *slog.Logger {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return o.logger
}

// CreateSnapshot captures the current state of files that will be changed.
// Secrets (secret:*) are skipped since Podman secrets have no versioning.
func CreateSnapshot(cs *reconciler.Changeset, diskReader DiskReader, opts ...Option) (*Snapshot, error) {
	if diskReader == nil {
		return nil, errors.New("rollback snapshot disk reader is nil")
	}
	log := loggerFrom(opts)

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
				log.Warn("snapshot: cannot read file, storing as absent",
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
// Collects all errors instead of aborting on first failure to maximize recovery.
func Restore(ctx context.Context, snap *Snapshot, writer applier.FileWriter, systemd applier.SystemdManager, opts ...Option) error {
	log := loggerFrom(opts)
	var errs []error
	for path, content := range snap.Files {
		if content == nil {
			log.Info("rollback: removing", "path", path)
			if err := writer.Remove(path); err != nil {
				log.Error("rollback: remove failed", "path", path, "error", err)
				errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
			}
			continue
		}

		log.Info("rollback: restoring", "path", path)
		if err := writer.WriteFile(path, content); err != nil {
			log.Error("rollback: restore failed", "path", path, "error", err)
			errs = append(errs, fmt.Errorf("restoring %s: %w", path, err))
		}
	}

	log.Info("rollback: running daemon-reload")
	if err := systemd.DaemonReload(ctx); err != nil {
		errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
	}

	return errors.Join(errs...)
}
