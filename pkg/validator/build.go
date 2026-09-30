package validator

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/buildunit"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// delivery is what the Fleet delivers as data files (files/, manifests/), in
// host-visible paths. picolet owns the directories of those categories, so a
// path under one of them that no file delivers is a Fleet mistake; any other
// path belongs to the operator and is not checked.
type delivery struct {
	roots []string
	files map[string]struct{}
}

func newDelivery(files []resolver.ResolvedFile, hostDataDir string) delivery {
	d := delivery{files: make(map[string]struct{})}
	if hostDataDir == "" {
		return d
	}
	for _, spec := range config.Specs() {
		if spec.Dest == config.DestData {
			d.roots = append(d.roots, spec.DataPath(hostDataDir, ""))
		}
	}
	for _, f := range files {
		spec, ok := config.SpecFor(f.Category)
		if ok && spec.Dest == config.DestData {
			d.files[spec.DataPath(hostDataDir, f.RelPath)] = struct{}{}
		}
	}
	return d
}

// owns reports whether path lies in a directory picolet delivers data files to.
func (d delivery) owns(path string) bool {
	for _, root := range d.roots {
		if isWithin(root, path) {
			return true
		}
	}
	return false
}

// isWithin reports whether path is dir or lies below it; both are clean.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (d delivery) delivers(path string) bool {
	_, ok := d.files[path]
	return ok
}

// deliversBelow reports whether a delivered file lies strictly below dir: a
// delivered file at dir itself is not a directory.
func (d delivery) deliversBelow(dir string) bool {
	for file := range d.files {
		if file != dir && isWithin(dir, file) {
			return true
		}
	}
	return false
}

// checkBuild reports the paths the .build unit at unitPath reads that lie in
// a delivered data directory but that the Fleet does not deliver: its
// Containerfile, its build context and its working directory. A path picolet
// cannot resolve from the unit (URL, systemd specifier, relative to the
// service's default directory) might exist on the Host and passes.
func (d delivery) checkBuild(unit *parser.UnitFile, unitPath string) error {
	setWorkDir, _ := unit.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
	workDir, _ := unit.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory)

	errs := []error{d.checkContainerfile(unit, buildunit.Resolve(unit, unitPath))}
	if !isWorkingDirectoryKeyword(setWorkDir) {
		errs = append(errs, d.checkDir(unit, quadlet.KeySetWorkingDirectory, setWorkDir))
	}
	errs = append(errs, d.checkDir(unit, quadlet.ServiceKeyWorkingDirectory, workDir))
	return errors.Join(errs...)
}

// checkContainerfile reports File= when every place podman build looks for
// it lies in a delivered data directory and none is delivered.
func (d delivery) checkContainerfile(unit *parser.UnitFile, paths buildunit.Paths) error {
	if !paths.ContainerfilesComplete {
		return nil
	}
	for _, c := range paths.Containerfiles {
		if !d.owns(c) || d.delivers(c) {
			return nil
		}
	}
	file, _ := unit.Lookup(quadlet.BuildGroup, quadlet.KeyFile)
	if len(paths.Containerfiles) == 1 && paths.Containerfiles[0] == filepath.Clean(file) {
		return notDelivered(unit, quadlet.KeyFile, file)
	}
	return fmt.Errorf("%s: %s=%s (%s) is not delivered by the Fleet",
		unit.Filename, quadlet.KeyFile, file, strings.Join(paths.Containerfiles, " or "))
}

// checkDir reports a directory the unit names under key when it lies in a
// delivered data directory but no delivered file lies below it.
func (d delivery) checkDir(unit *parser.UnitFile, key, dir string) error {
	if !checkable(dir) {
		return nil
	}
	path := filepath.Clean(dir)
	if !d.owns(path) || d.deliversBelow(path) {
		return nil
	}
	return notDelivered(unit, key, dir)
}

func notDelivered(unit *parser.UnitFile, key, value string) error {
	return fmt.Errorf("%s: %s=%s is not delivered by the Fleet", unit.Filename, key, value)
}

// checkable reports whether a reference is a literal absolute path. Podman
// treats anything else as a URL or resolves it relative to the unit file,
// and systemd expands specifiers (%h) only on the Host.
func checkable(ref string) bool {
	return filepath.IsAbs(ref) && !strings.Contains(ref, "%")
}

// isWorkingDirectoryKeyword reports the SetWorkingDirectory= values that name
// a file's directory instead of a path.
func isWorkingDirectoryKeyword(value string) bool {
	switch strings.ToLower(value) {
	case "file", "unit":
		return true
	}
	return false
}
