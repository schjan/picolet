// Package buildunit resolves the local paths a .build Quadlet's image build
// reads, the way Podman does: Quadlet's ConvertBuild derives the build
// service's working directory and the podman build context argument, and
// podman build derives the context directory and the Containerfile paths it
// tries. The validator checks these paths against the delivered files; the
// applier rebuilds when one of them changes.
package buildunit

import (
	"cmp"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
)

// Paths are the local paths podman build reads for a .build unit.
type Paths struct {
	// Context is the build context directory; "" when it is remote (a URL)
	// or depends on something the unit does not determine (a systemd
	// specifier, the service's default working directory).
	Context string
	// Containerfiles are the paths podman build tries for File=, in order
	// (buildah's BuildDockerfiles): File= resolved against the working
	// directory, then File= joined onto the context unless it already starts
	// with it. Only the resolvable ones are listed.
	Containerfiles []string
	// ContainerfilesComplete reports that Containerfiles lists every path
	// podman build may try: File= is set, local, and each candidate resolves.
	ContainerfilesComplete bool
}

// Resolve returns the paths podman build reads for the .build unit u, which
// lies at unitPath on the Host.
func Resolve(u *parser.UnitFile, unitPath string) Paths {
	file, _ := u.Lookup(quadlet.BuildGroup, quadlet.KeyFile)
	setWorkDir, _ := u.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
	workDir, _ := u.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory)

	contextArg, workDir := setWorkingDirectory(setWorkDir, workDir, file, filepath.Dir(unitPath))
	if contextArg == "" && !strings.HasPrefix(file, "%") && !filepath.IsAbs(file) && !isURL(file) {
		// ConvertBuild passes the working directory as the context of a
		// relative File= (and of none).
		contextArg = workDir
	}
	if contextArg == "" {
		return withoutContext(file, workDir)
	}
	return withContext(file, contextArg, workDir)
}

// setWorkingDirectory returns the context argument ConvertBuild passes to
// podman build for SetWorkingDirectory= ("" for none) and the build
// service's working directory ("" for systemd's default), following Quadlet's
// handleSetWorkingDirectory: an explicit [Service] WorkingDirectory= wins
// over the one SetWorkingDirectory= derives, and drops a unit-relative
// SetWorkingDirectory= path.
func setWorkingDirectory(setWorkDir, workDir, file, unitDir string) (contextArg, serviceWorkDir string) {
	switch strings.ToLower(setWorkDir) {
	case "":
		return "", workDir
	case "file":
		if workDir == "" && file != "" {
			return "", filepath.Dir(unitRelative(file, unitDir))
		}
		return "", workDir
	case "unit":
		return "", cmp.Or(workDir, unitDir)
	}
	switch {
	case filepath.IsAbs(setWorkDir) || isURL(setWorkDir):
		return setWorkDir, workDir
	case workDir != "":
		return "", workDir
	default:
		return setWorkDir, unitDir
	}
}

// withoutContext returns the paths of a build podman runs without a context
// argument: it uses the Containerfile's directory as the context.
func withoutContext(file, workDir string) Paths {
	if file == "" || isURL(file) {
		return Paths{}
	}
	f, ok := resolve(file, workDir)
	if !ok {
		return Paths{}
	}
	return Paths{Context: filepath.Dir(f), Containerfiles: []string{f}, ContainerfilesComplete: true}
}

// withContext returns the paths of a build podman runs with the context
// argument contextArg.
func withContext(file, contextArg, workDir string) Paths {
	var p Paths
	if !isURL(contextArg) {
		p.Context, _ = resolve(contextArg, workDir)
	}
	if file == "" || isURL(file) {
		return p
	}
	first, ok := resolve(file, workDir)
	if ok {
		p.Containerfiles = []string{first}
	}
	if p.Context == "" || strings.Contains(file, "%") {
		return p
	}
	p.ContainerfilesComplete = ok
	if joined := filepath.Join(p.Context, file); !strings.HasPrefix(file, p.Context) && joined != first {
		p.Containerfiles = append(p.Containerfiles, joined)
	}
	return p
}

// unitRelative resolves p against the unit's directory like Quadlet's
// getAbsolutePath: absolute paths and systemd specifiers stay as they are.
func unitRelative(p, unitDir string) string {
	if strings.HasPrefix(p, "%") || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(unitDir, p)
}

// resolve returns p as an absolute clean path, a relative one against the
// working directory; ok is false when it contains a systemd specifier or is
// relative to a working directory the unit does not set.
func resolve(p, workDir string) (string, bool) {
	if strings.Contains(p, "%") {
		return "", false
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), true
	}
	if !filepath.IsAbs(workDir) || strings.Contains(workDir, "%") {
		return "", false
	}
	return filepath.Join(workDir, p), true
}

// isURL reports whether Quadlet treats a File= or SetWorkingDirectory= value
// as a URL.
func isURL(p string) bool {
	return quadlet.URL.MatchString(p)
}
