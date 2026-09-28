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

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/state"
)

// Scanner detects and removes files that were deployed by picolet but are no longer
// tracked in the current managed-files state (e.g. after a state reset or partial apply failure).
type Scanner struct {
	writer     applier.FileWriter
	podman     applier.PodmanClient
	systemd    applier.SystemdManager
	quadletDir string
	systemdDir string
	dataDir    string
}

// New creates a new Scanner.
func New(writer applier.FileWriter, podman applier.PodmanClient, systemd applier.SystemdManager, quadletDir, systemdDir, dataDir string) *Scanner {
	return &Scanner{
		writer:     writer,
		podman:     podman,
		systemd:    systemd,
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
	guard := s.readPodGuard()
	removed, err := s.scanOwnedDir(s.quadletDir, managedFiles, func(path string) { s.stopGeneratedUnit(ctx, path, guard) })
	result.FilesRemoved += removed
	if err != nil {
		return result, err
	}
	for _, spec := range config.Specs() {
		if spec.Dest != config.DestData {
			continue
		}
		removed, err := s.scanOwnedDir(filepath.Join(s.dataDir, spec.Subdir), managedFiles, nil)
		result.FilesRemoved += removed
		if err != nil {
			return result, err
		}
	}
	removed, err = s.scanMarkedDir(s.systemdDir, managedFiles)
	result.FilesRemoved += removed
	if err != nil {
		return result, err
	}
	secretsRemoved, err := s.scanSecrets(ctx, managedFiles)
	result.SecretsRemoved += secretsRemoved
	return result, err
}

// scanOwnedDir removes any file in a picolet-owned directory that is absent from managedFiles,
// calling beforeRemove (if set) on each orphan first.
// Uses WalkDir so nested data subdirectories are covered.
func (s *Scanner) scanOwnedDir(dir string, managedFiles map[string]state.ManagedFile, beforeRemove func(path string)) (int, error) {
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
			if beforeRemove != nil {
				beforeRemove(path)
			}
			if s.removeOrphan(path) {
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// stopGeneratedUnit stops the service Podman generated from an orphaned Quadlet
// file: removing the file and reloading only drops the unit definition and
// leaves the service running. The agent's own unit is never stopped, nor is a
// pod the guard protects: stopping a pod stops its members (BindsTo=), the
// agent included. Like the applier's pre-delete stop, this is best-effort: a
// failure is logged and the file is removed regardless, because StopUnit also
// fails for a unit systemd never loaded, and keeping the file would make such
// an orphan permanent.
func (s *Scanner) stopGeneratedUnit(ctx context.Context, path string, guard podGuard) {
	category, ok := config.CategoryForExtension(filepath.Ext(path))
	if spec, _ := config.SpecFor(category); !ok || spec.Unit != config.GeneratedUnit {
		return
	}
	unit, err := parser.ParseUnitFile(path)
	if err != nil {
		slog.Warn("orphan scan: cannot parse quadlet, its service is not stopped", "path", path, "error", err)
		return
	}
	name, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		slog.Warn("orphan scan: cannot derive service name, not stopped", "path", path, "error", err)
		return
	}
	service := name + ".service"
	if config.IsDefaultSelfUnit(service) {
		return
	}
	if category == config.CategoryPod && guard.protects(filepath.Base(path)) {
		slog.Warn("orphaned pod may be joined by the agent's own container, not stopping it", "path", path, "unit", service)
		return
	}
	slog.Warn("orphaned quadlet detected, stopping its service", "path", path, "unit", service)
	if err := s.systemd.StopUnit(ctx, service); err != nil {
		slog.Error("stopping orphaned service failed", "unit", service, "error", err)
	}
}

// podGuard records which pods the agent's own container joins.
type podGuard struct {
	// agentPods are the pod files ("web.pod") an agent container names in Pod=.
	agentPods map[string]struct{}
	// blind is set when some .container could not be read or parsed: any pod
	// might then be the agent's, so none is stopped.
	blind bool
}

func (g podGuard) protects(podFile string) bool {
	_, joined := g.agentPods[podFile]
	return joined || g.blind
}

// readPodGuard reads every container Quadlet in the Quadlet directory, managed
// or not, for the pods the agent's own container joins. It runs before any
// orphan is removed, so a stale agent container still protects its pod. The
// validator rejects such a Fleet; this guards files deployed before that check
// or after a state reset. It fails closed: an unreadable directory entry or
// container makes the guard blind.
func (s *Scanner) readPodGuard() podGuard {
	guard := podGuard{agentPods: make(map[string]struct{})}
	err := filepath.WalkDir(s.quadletDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if category, _ := config.CategoryForExtension(filepath.Ext(path)); d.IsDir() || category != config.CategoryContainer {
			return nil
		}
		unit, err := parser.ParseUnitFile(path)
		if err != nil {
			return err
		}
		name, err := quadlet.GetUnitServiceName(unit)
		if err != nil {
			return err
		}
		if pod, _ := unit.Lookup(quadlet.ContainerGroup, quadlet.KeyPod); pod != "" && config.IsDefaultSelfUnit(name+".service") {
			guard.agentPods[pod] = struct{}{}
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("orphan scan: cannot read every container quadlet, no pod will be stopped", "error", err)
		guard.blind = true
	}
	return guard
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
	buf := make([]byte, len(config.PicoletMarker))
	_, err = io.ReadFull(f, buf)
	return err == nil && string(buf) == config.PicoletMarker
}
