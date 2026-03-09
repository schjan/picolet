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
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
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

// ScanResult contains counts of resources removed during an orphan scan.
type ScanResult struct {
	FilesRemoved   int
	SecretsRemoved int
}

// Scan removes any file or secret that was placed by picolet but is absent from managedFiles.
// Individual deletion errors are logged and do not abort the scan.
// Directory-scan errors are returned because they indicate a systemic problem.
func (s *Scanner) Scan(ctx context.Context, managedFiles map[string]state.ManagedFile) (ScanResult, error) {
	var result ScanResult
	r1, err := s.scanOwnedDir(s.quadletDir, managedFiles)
	result.FilesRemoved += r1
	if err != nil {
		return result, err
	}
	r2, err := s.scanOwnedDir(filepath.Join(s.dataDir, "manifests"), managedFiles)
	result.FilesRemoved += r2
	if err != nil {
		return result, err
	}
	r3, err := s.scanMarkedDir(s.systemdDir, managedFiles)
	result.FilesRemoved += r3
	if err != nil {
		return result, err
	}
	r4, err := s.scanSecrets(ctx, managedFiles)
	result.SecretsRemoved += r4
	return result, err
}

// scanOwnedDir removes any file in a picolet-owned directory that is absent from managedFiles.
// Uses WalkDir so nested manifest subdirectories are covered.
func (s *Scanner) scanOwnedDir(dir string, managedFiles map[string]state.ManagedFile) (int, error) {
	var removed int
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
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// scanMarkedDir scans a shared directory (systemd) and removes only files that carry
// the picolet marker and are absent from managedFiles. Non-picolet files are untouched.
func (s *Scanner) scanMarkedDir(dir string, managedFiles map[string]state.ManagedFile) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading systemd dir %s: %w", dir, err)
	}
	var removed int
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
				removed++
			}
		}
	}
	return removed, nil
}

// scanSecrets removes Podman secrets that carry the managed-by=picolet label but are
// absent from managedFiles.
func (s *Scanner) scanSecrets(ctx context.Context, managedFiles map[string]state.ManagedFile) (int, error) {
	names, err := s.podman.ListManagedSecrets(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing managed secrets: %w", err)
	}
	var removed int
	for _, name := range names {
		if _, managed := managedFiles["secret:"+name]; !managed {
			slog.Warn("orphaned secret detected, removing", "name", name)
			if err := s.podman.SecretRemove(ctx, name); err != nil {
				slog.Error("removing orphaned secret failed", "name", name, "error", err)
			} else {
				removed++
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
		slog.Warn("orphan scan: cannot open file for marker check", "path", path, "error", err)
		return false
	}
	defer f.Close()
	buf := make([]byte, len(resolver.PicoletMarker))
	_, err = io.ReadFull(f, buf)
	return err == nil && string(buf) == resolver.PicoletMarker
}
