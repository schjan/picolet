package orphan

import (
	"context"
	"errors"
	"fmt"
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
func (s *Scanner) Scan(ctx context.Context, managedFiles map[string]string) error {
	if err := s.scanOwnedDir(s.quadletDir, managedFiles); err != nil {
		return err
	}
	if err := s.scanOwnedDir(filepath.Join(s.dataDir, "manifests"), managedFiles); err != nil {
		return err
	}
	if err := s.scanMarkedDir(s.systemdDir, managedFiles); err != nil {
		return err
	}
	return s.scanSecrets(ctx, managedFiles)
}

// scanOwnedDir removes any file in a picolet-owned directory that is absent from managedFiles.
// Uses WalkDir so nested manifest subdirectories are covered.
func (s *Scanner) scanOwnedDir(dir string, managedFiles map[string]string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll // dir not yet created, nothing to clean up
			}
			return fmt.Errorf("scanning %s: %w", dir, err)
		}
		if d.IsDir() {
			return nil
		}
		if _, managed := managedFiles[path]; !managed {
			s.removeOrphan(path)
		}
		return nil
	})
}

// scanMarkedDir scans a shared directory (systemd) and removes only files that carry
// the picolet marker and are absent from managedFiles. Non-picolet files are untouched.
func (s *Scanner) scanMarkedDir(dir string, managedFiles map[string]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading systemd dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !hasPicoletMarker(path) {
			continue
		}
		if _, managed := managedFiles[path]; !managed {
			s.removeOrphan(path)
		}
	}
	return nil
}

// scanSecrets removes Podman secrets that carry the managed-by=picolet label but are
// absent from managedFiles.
func (s *Scanner) scanSecrets(ctx context.Context, managedFiles map[string]string) error {
	names, err := s.podman.ListManagedSecrets(ctx)
	if err != nil {
		return fmt.Errorf("listing managed secrets: %w", err)
	}
	for _, name := range names {
		if _, managed := managedFiles["secret:"+name]; !managed {
			slog.Warn("orphaned secret detected, removing", "name", name)
			if err := s.podman.SecretRemove(ctx, name); err != nil {
				slog.Error("removing orphaned secret failed", "name", name, "error", err)
			}
		}
	}
	return nil
}

func (s *Scanner) removeOrphan(path string) {
	slog.Warn("orphaned file detected, removing", "path", path)
	if err := s.writer.Remove(path); err != nil {
		slog.Error("removing orphaned file failed", "path", path, "error", err)
	}
}

// hasPicoletMarker reports whether the first bytes of a file match the picolet marker.
func hasPicoletMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(applier.PicoletMarker))
	n, _ := f.Read(buf)
	return string(buf[:n]) == applier.PicoletMarker
}
