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
	paths := buildunit.Resolve(unit, unitPath)
	errs := []error{d.checkContainerfile(unit, paths)}
	if paths.NamedContext {
		setWorkDir, _ := unit.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
		errs = append(errs, d.checkDir(unit, quadlet.KeySetWorkingDirectory, setWorkDir, paths.Context))
	}
	// systemd fails the service when it cannot chdir into it.
	if workDir, _ := unit.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory); filepath.IsAbs(workDir) && !strings.Contains(workDir, "%") {
		errs = append(errs, d.checkDir(unit, quadlet.ServiceKeyWorkingDirectory, workDir, filepath.Clean(workDir)))
	}
	return errors.Join(errs...)
}

// checkContainerfile reports the Containerfile when every place podman build
// looks for it lies in a delivered data directory and none is delivered.
// Without File=, podman build looks in the context; a context the Fleet
// delivers nothing to is reported as a directory instead.
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
	switch {
	case file == "":
		if !d.deliversBelow(paths.Context) {
			return nil
		}
		return fmt.Errorf("%s: no %s=, and neither %s is delivered by the Fleet",
			unit.Filename, quadlet.KeyFile, strings.Join(paths.Containerfiles, " nor "))
	case len(paths.Containerfiles) == 1 && paths.Containerfiles[0] == filepath.Clean(file):
		return notDelivered(unit, quadlet.KeyFile, file, "")
	default:
		return notDelivered(unit, quadlet.KeyFile, file, strings.Join(paths.Containerfiles, " or "))
	}
}

// checkDir reports the directory dir the unit names as key=value when it
// lies in a delivered data directory but no delivered file lies below it.
func (d delivery) checkDir(unit *parser.UnitFile, key, value, dir string) error {
	if !d.owns(dir) || d.deliversBelow(dir) {
		return nil
	}
	if dir == value {
		return notDelivered(unit, key, value, "")
	}
	return notDelivered(unit, key, value, dir)
}

// notDelivered reports key=value, resolved to resolved when that differs.
func notDelivered(unit *parser.UnitFile, key, value, resolved string) error {
	if resolved != "" {
		value += " (" + resolved + ")"
	}
	return fmt.Errorf("%s: %s=%s is not delivered by the Fleet", unit.Filename, key, value)
}
