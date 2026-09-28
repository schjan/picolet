package orphan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

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
	// unitDirs are the directories Podman's generator reads units and
	// drop-ins from; quadletDir is always searched too.
	unitDirs []string
}

// Option configures a Scanner.
type Option func(*Scanner)

// WithUnitDirs sets the directories Podman's generator reads Quadlet units and
// their drop-ins from (quadlet.GetUnitDirs). Without it only the Quadlet
// directory is searched.
func WithUnitDirs(dirs ...string) Option {
	return func(s *Scanner) {
		s.unitDirs = slices.Clone(dirs)
	}
}

// New creates a new Scanner.
func New(writer applier.FileWriter, podman applier.PodmanClient, systemd applier.SystemdManager, quadletDir, systemdDir, dataDir string, opts ...Option) *Scanner {
	s := &Scanner{
		writer:     writer,
		podman:     podman,
		systemd:    systemd,
		quadletDir: quadletDir,
		systemdDir: systemdDir,
		dataDir:    dataDir,
	}
	for _, opt := range opts {
		opt(s)
	}
	if !slices.Contains(s.unitDirs, quadletDir) {
		s.unitDirs = append(s.unitDirs, quadletDir)
	}
	return s
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
	view := s.readQuadletView()
	removed, err := s.scanOwnedDir(s.quadletDir, managedFiles, func(path string) { s.stopGeneratedUnit(ctx, path, view) })
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
		if path == dir && !d.IsDir() {
			// WalkDir does not follow a symlinked root; without this the link
			// itself would be taken for an orphaned file and removed.
			return fmt.Errorf("scanning %s: is a symlink or not a directory, orphans in it are not removed", dir)
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
// file whose category has StopOrphan: removing the file and reloading only
// drops the unit definition and leaves the service running. Nothing is stopped
// unless the file is the one Podman generates its unit from (see
// readQuadletView), nor the agent's own unit, nor a pod the agent's own
// container may join: stopping a pod stops its members (BindsTo=), the agent
// included. Like the applier's pre-delete stop, this is
// best-effort: a failure is logged and the file is removed regardless, because
// StopUnit also fails for a unit systemd never loaded, and keeping the file
// would make such an orphan permanent.
func (s *Scanner) stopGeneratedUnit(ctx context.Context, path string, view quadletView) {
	service, category, ok := s.generatedService(path, view)
	if !ok || config.IsDefaultSelfUnit(service) {
		return
	}
	if category == config.CategoryPod && view.protectsPod(filepath.Base(path)) {
		slog.Warn("orphaned pod may be joined by the agent's own container, not stopping it", "path", path, "unit", service)
		return
	}
	slog.Warn("orphaned quadlet detected, stopping its service", "path", path, "unit", service)
	if err := s.systemd.StopUnit(ctx, service); err != nil {
		slog.Error("stopping orphaned service failed", "unit", service, "error", err)
	}
}

// generatedService returns the service Podman generates from the Quadlet at
// path and its category, or false if its category is not stopped as an Orphan,
// path is not the file Podman generates its unit from, or it cannot be read.
func (s *Scanner) generatedService(path string, view quadletView) (string, config.Category, bool) {
	category, _ := config.CategoryForExtension(filepath.Ext(path))
	if spec, _ := config.SpecFor(category); !spec.StopOrphan {
		return "", "", false
	}
	if selected := view.units[filepath.Base(path)]; !sameEntry(selected, path) {
		slog.Warn("orphaned quadlet is not the file Podman generates this unit from, its service is not stopped",
			"path", path, "selected", selected)
		return "", "", false
	}
	loaded, err := s.loadUnit(path)
	if err != nil {
		slog.Warn("orphan scan: cannot read quadlet, its service is not stopped", "path", path, "error", err)
		return "", "", false
	}
	if loaded.dropInErr != nil {
		// Podman generates the unit anyway, from the drop-ins it could read.
		slog.Warn("orphan scan: some drop-ins could not be read", "path", path, "error", loaded.dropInErr)
	}
	return loaded.service, category, true
}

// sameEntry reports whether a and b are the same directory entry: same name in
// the same directory once the directories' symlinks are resolved. The entries
// themselves are not resolved: to Podman a symlink or hard link in another
// directory is a separate unit file.
func sameEntry(a, b string) bool {
	if filepath.Base(a) != filepath.Base(b) {
		return false
	}
	da, errA := filepath.EvalSymlinks(filepath.Dir(a))
	db, errB := filepath.EvalSymlinks(filepath.Dir(b))
	return errA == nil && errB == nil && da == db
}

// quadletView is read once before any orphan is removed: units holds the
// StopOrphan units as Podman's generator selects them, agentPods covers every
// container file.
type quadletView struct {
	// units maps each unit filename of a StopOrphan category ("web.pod") to the
	// file Podman generates it from: the first file of that name that parses,
	// unit dirs in order.
	units map[string]string
	// agentPods are the pod files ("web.pod") an agent container names in Pod=.
	agentPods map[string]struct{}
	// blind is set when a container's settings (the unit or one of its
	// drop-ins) or a unit directory could not be read: any pod might then be
	// the agent's, so none is stopped.
	blind bool
}

func (v quadletView) protectsPod(podFile string) bool {
	_, joined := v.agentPods[podFile]
	return joined || v.blind
}

// readQuadletView reads the unit directories as Podman's generator does
// (cmd/quadlet loadUnitsFromDir): in order, not recursively (the list already
// holds subdirectories), a name going to the first file of that name that
// parses. Every container file, even one shadowed by a same-named file, is
// merged with its drop-ins (they may set ServiceName= or Pod=) and checked
// for being the agent's: the running agent may come from any of them, since
// files can change after the generator last ran. An orphaned agent container
// still protects its pod, since this runs before any orphan is removed. The
// validator rejects such a Fleet; this guards files deployed before that
// check, placed by hand, or left after a state reset. It fails closed on
// anything that could hide the agent's container.
func (s *Scanner) readQuadletView() quadletView {
	view := quadletView{units: make(map[string]string), agentPods: make(map[string]struct{})}
	var errs []error
	for _, dir := range s.unitDirs {
		entries, err := readDirIfExists(dir)
		errs = append(errs, err)
		for _, entry := range entries {
			errs = append(errs, s.readUnitEntry(filepath.Join(dir, entry.Name()), &view))
		}
	}
	if err := errors.Join(errs...); err != nil {
		slog.Warn("orphan scan: cannot read every container's settings, no pod will be stopped", "error", err)
		view.blind = true
	}
	return view
}

// readUnitEntry adds the unit file at path to view: every container is checked
// for being the agent's, whatever its StopOrphan flag, and a unit of a
// StopOrphan category claims its name if still free and it parses. The error
// covers containers only; other unparseable units are just logged.
func (s *Scanner) readUnitEntry(path string, view *quadletView) error {
	name := filepath.Base(path)
	if !quadlet.IsExtSupported(name) {
		return nil
	}
	category, _ := config.CategoryForExtension(filepath.Ext(name))
	spec, _ := config.SpecFor(category)
	_, claimed := view.units[name]
	if category == config.CategoryContainer {
		return s.readContainerEntry(path, !claimed && spec.StopOrphan, view)
	}
	if claimed || !spec.StopOrphan {
		return nil
	}
	if _, err := parser.ParseUnitFile(path); err != nil {
		slog.Warn("orphan scan: cannot parse quadlet", "path", path, "error", err)
		return nil
	}
	view.units[name] = path
	return nil
}

// readContainerEntry loads the container at path, records the pod it joins if
// it is the agent's, and claims its name if claim is set.
func (s *Scanner) readContainerEntry(path string, claim bool, view *quadletView) error {
	loaded, err := s.loadUnit(path)
	if err != nil {
		return err
	}
	if claim {
		view.units[filepath.Base(path)] = path
	}
	if pod, _ := loaded.unit.Lookup(quadlet.ContainerGroup, quadlet.KeyPod); pod != "" && config.IsDefaultSelfUnit(loaded.service) {
		view.agentPods[pod] = struct{}{}
	}
	return loaded.dropInErr
}

// readDirIfExists lists dir; a missing dir is empty, not an error.
func readDirIfExists(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	return entries, nil
}

// loadedUnit is a Quadlet merged with its drop-ins.
type loadedUnit struct {
	unit    *parser.UnitFile
	service string // the generated service, e.g. "web-pod.service"
	// dropInErr reports drop-ins that could not be read; like Podman, the
	// unit is still merged from the others.
	dropInErr error
}

// loadUnit parses the Quadlet at path and merges its drop-ins as Podman does.
// The error is set only if the file itself cannot be read or named.
func (s *Scanner) loadUnit(path string) (loadedUnit, error) {
	unit, err := parser.ParseUnitFile(path)
	if err != nil {
		return loadedUnit{}, fmt.Errorf("%s: %w", path, err)
	}
	dropInErr := mergeDropIns(unit, s.unitDirs)
	if dropInErr != nil {
		dropInErr = fmt.Errorf("%s: %w", path, dropInErr)
	}
	name, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return loadedUnit{}, fmt.Errorf("%s: %w", path, err)
	}
	return loadedUnit{unit: unit, service: name + ".service", dropInErr: dropInErr}, nil
}

// mergeDropIns merges the unit's drop-ins into it the way Podman's generator
// does (cmd/quadlet loadUnitDropins): every drop-in directory name that applies
// to the unit (GetUnitDropinPaths, most specific first) is looked up in every
// unit directory, the first .conf of each name wins (before parsing), and they
// merge in name order. Like Podman it merges what it can read and reports the
// rest.
func mergeDropIns(unit *parser.UnitFile, unitDirs []string) error {
	byName := make(map[string]string)
	var errs []error
	for _, dropInDir := range unit.GetUnitDropinPaths() {
		for _, unitDir := range unitDirs {
			dir := filepath.Join(unitDir, dropInDir)
			entries, err := readDirIfExists(dir)
			errs = append(errs, err)
			for _, entry := range entries {
				if _, seen := byName[entry.Name()]; !seen && filepath.Ext(entry.Name()) == ".conf" {
					byName[entry.Name()] = filepath.Join(dir, entry.Name())
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		dropIn, err := parser.ParseUnitFile(byName[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("drop-in %s: %w", byName[name], err))
			continue
		}
		unit.Merge(dropIn)
	}
	return errors.Join(errs...)
}

// scanMarkedDir scans a shared directory (systemd) and removes only files that carry
// the picolet marker and are absent from managedFiles. Non-picolet files are untouched.
func (s *Scanner) scanMarkedDir(dir string, managedFiles map[string]state.ManagedFile) (int, error) {
	entries, err := readDirIfExists(dir)
	if err != nil {
		return 0, err
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
