package validator

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

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

// reference is a path a .build unit reads: its key, the value as written,
// and the path it resolves to (value when absolute).
type reference struct {
	key, value, path string
}

// checkBuild reports the paths a .build unit reads that lie in a delivered
// data directory but that the Fleet does not deliver.
func (d delivery) checkBuild(unit *parser.UnitFile) error {
	containerfile, context, workDir := buildReferences(unit)
	return errors.Join(
		d.check(unit, containerfile, d.delivers),
		d.check(unit, context, d.deliversUnder),
		d.check(unit, workDir, d.deliversUnder),
	)
}

// buildReferences returns the paths a .build unit reads, following Podman's
// ConvertBuild and podman build:
//   - containerfile: File=. podman build looks a relative local one up in
//     the service's working directory, then in the build context. It
//     resolves against [Service] WorkingDirectory= when that is also the
//     context (no SetWorkingDirectory= path), and against the context when
//     no working directory is set; with both set and different, either may
//     hold it, so it is not resolved.
//   - context: a SetWorkingDirectory= path. Without one the context is the
//     working directory or the absolute Containerfile's directory, both
//     checked on their own; SetWorkingDirectory=file/unit without an
//     explicit working directory resolve relative to the unit file, never
//     into a data directory.
//   - workDir: [Service] WorkingDirectory=, which systemd enters before the
//     build runs.
func buildReferences(unit *parser.UnitFile) (containerfile, context, workDir reference) {
	file, _ := unit.Lookup(quadlet.BuildGroup, quadlet.KeyFile)
	setWorkDir, _ := unit.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
	dir, _ := unit.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory)

	containerfile = reference{key: quadlet.KeyFile, value: file, path: file}
	workDir = reference{key: quadlet.ServiceKeyWorkingDirectory, value: dir, path: dir}
	lookupDir := dir
	if setWorkDir != "" && !isWorkingDirectoryKeyword(setWorkDir) {
		context = reference{key: quadlet.KeySetWorkingDirectory, value: setWorkDir, path: setWorkDir}
		if dir != "" {
			lookupDir = ""
		} else {
			lookupDir = setWorkDir
		}
	}
	if isRelativeLocal(file) && checkable(lookupDir) {
		containerfile.path = filepath.Join(lookupDir, file)
	}
	return containerfile, context, workDir
}

// podmanURL is Podman's pattern for a File=/SetWorkingDirectory= value it
// treats as a URL (quadlet.URL, v5 pkg/systemd/quadlet/quadlet.go).
var podmanURL = regexp.MustCompile(`^((https?)|(git)://)|(github\.com/).+$`)

// isRelativeLocal reports a File= that Podman passes on relative to the
// working directory: set, not absolute, not a URL, not a systemd specifier.
func isRelativeLocal(file string) bool {
	return file != "" && !filepath.IsAbs(file) && !strings.HasPrefix(file, "%") && !podmanURL.MatchString(file)
}

// check reports ref when it lies in a delivered data directory but
// delivered does not accept it. A reference that is not an absolute path
// (URL, systemd specifier, relative to the unit file) is not checked.
func (d delivery) check(unit *parser.UnitFile, ref reference, delivered func(string) bool) error {
	if !checkable(ref.path) {
		return nil
	}
	path := filepath.Clean(ref.path)
	if !d.owns(path) || delivered(path) {
		return nil
	}
	if filepath.Clean(ref.value) == path {
		return fmt.Errorf("%s: %s=%s is not delivered by the Fleet", unit.Filename, ref.key, ref.value)
	}
	return fmt.Errorf("%s: %s=%s (%s) is not delivered by the Fleet", unit.Filename, ref.key, ref.value, path)
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

func (d delivery) delivers(path string) bool {
	_, ok := d.files[path]
	return ok
}

// deliversUnder reports whether a delivered file is dir or lies below it.
func (d delivery) deliversUnder(dir string) bool {
	for file := range d.files {
		if isWithin(dir, file) {
			return true
		}
	}
	return false
}
