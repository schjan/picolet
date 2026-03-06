package orphan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/schjan/picolet/pkg/applier"
)

// Scanner detects and removes files that were deployed by picolet but are no longer
// tracked in the current managed-files state (e.g. after a state reset or partial apply failure).
type Scanner struct {
	writer     applier.FileWriter
	podman     applier.PodmanClient
	quadletDir string
	systemdDir string
	dataDir    string
}

// New creates a new Scanner.
func New(writer applier.FileWriter, podman applier.PodmanClient, quadletDir, systemdDir, dataDir string) *Scanner {
	return &Scanner{
		writer:     writer,
		podman:     podman,
		quadletDir: quadletDir,
		systemdDir: systemdDir,
		dataDir:    dataDir,
	}
}

// Scan removes any file or secret that was placed by picolet but is absent from managedFiles.
// Individual deletion errors are logged and do not abort the scan.
// Directory-scan errors are returned because they indicate a systemic problem.
// Returns true if any resource was successfully removed.
func (s *Scanner) Scan(ctx context.Context, managedFiles map[string]string) (bool, error) {
	r1, err := s.scanOwnedDir(s.quadletDir, managedFiles)
	if err != nil {
		return r1, err
	}
	r2, err := s.scanOwnedDir(filepath.Join(s.dataDir, "manifests"), managedFiles)
	if err != nil {
		return r1 || r2, err
	}
	r3, err := s.scanMarkedDir(s.systemdDir, managedFiles)
	if err != nil {
		return r1 || r2 || r3, err
	}
	r4, err := s.scanSecrets(ctx, managedFiles)
	return r1 || r2 || r3 || r4, err
}

// scanOwnedDir removes any file in a picolet-owned directory that is absent from managedFiles.
// Uses WalkDir so nested manifest subdirectories are covered.
func (s *Scanner) scanOwnedDir(dir string, managedFiles map[string]string) (bool, error) {
	var removed bool
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if path == dir {
					return filepath.SkipAll // root dir not yet created, nothing to clean up
				}
				return nil // nested entry vanished during walk, skip it
			}
			return fmt.Errorf("scanning %s: %w", dir, err)
		}
		if d.IsDir() {
			return nil
		}
		if _, managed := managedFiles[path]; !managed {
			if s.removeOrphan(path) {
				removed = true
			}
		}
		return nil
	})
	return removed, err
}

// scanMarkedDir scans a shared directory (systemd) and removes only files that carry
// the picolet marker and are absent from managedFiles. Non-picolet files are untouched.
func (s *Scanner) scanMarkedDir(dir string, managedFiles map[string]string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading systemd dir %s: %w", dir, err)
	}
	var removed bool
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !hasPicoletMarker(path) {
			continue
		}
		if _, managed := managedFiles[path]; !managed {
			if s.removeOrphan(path) {
				removed = true
			}
		}
	}
	return removed, nil
}

// scanSecrets removes Podman secrets that carry the managed-by=picolet label but are
// absent from managedFiles.
func (s *Scanner) scanSecrets(ctx context.Context, managedFiles map[string]string) (bool, error) {
	names, err := s.podman.ListManagedSecrets(ctx)
	if err != nil {
		return false, fmt.Errorf("listing managed secrets: %w", err)
	}
	var removed bool
	for _, name := range names {
		if _, managed := managedFiles["secret:"+name]; !managed {
			slog.Warn("orphaned secret detected, removing", "name", name)
			if err := s.podman.SecretRemove(ctx, name); err != nil {
				slog.Error("removing orphaned secret failed", "name", name, "error", err)
			} else {
				removed = true
			}
		}
	}
	return removed, nil
}

func (s *Scanner) removeOrphan(path string) bool {
	slog.Warn("orphaned file detected, removing", "path", path)
	if err := s.writer.Remove(path); err != nil {
		slog.Error("removing orphaned file failed", "path", path, "error", err)
		return false
	}
	return true
}

// hasPicoletMarker reports whether the first bytes of a file match the picolet marker.
func hasPicoletMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(applier.PicoletMarker))
	_, err = io.ReadFull(f, buf)
	return err == nil && string(buf) == applier.PicoletMarker
}
