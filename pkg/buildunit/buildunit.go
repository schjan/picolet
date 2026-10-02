// Package buildunit resolves the local paths a .build Quadlet's image build
// reads, in Podman's two stages: Quadlet's ConvertBuild derives the build
// service's working directory and the podman build context argument, and
// podman build derives the context directory and the Containerfile paths it
// tries. Each stage has its own notion of a URL. The validator checks these
// paths against the delivered files; the applier rebuilds when one of them
// changes.
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
	// Context is the build context directory; "" when it is remote (a URL,
	// stdin) or depends on something the unit does not determine (a systemd
	// specifier, the service's default working directory).
	Context string
	// NamedContext reports that Context is the path SetWorkingDirectory=
	// names, not one derived from File= or the working directory.
	NamedContext bool
	// Containerfiles are the paths podman build tries for File=, in order
	// (buildah's BuildDockerfiles): File= resolved against the working
	// directory, then File= joined onto the context unless it already starts
	// with it. Without File=, the context's Containerfile and Dockerfile.
	// Only the resolvable ones are listed.
	Containerfiles []string
	// ContainerfilesComplete reports that Containerfiles lists every path
	// podman build may try: the Containerfile is local (not a URL or stdin)
	// and each candidate resolves.
	ContainerfilesComplete bool
}

// Resolve returns the paths podman build reads for the .build unit u.
// unitPath is where the unit file lies: a unit-relative
// SetWorkingDirectory= and =unit resolve against its directory.
func Resolve(u *parser.UnitFile, unitPath string) Paths {
	file, _ := u.Lookup(quadlet.BuildGroup, quadlet.KeyFile)
	setWorkDir, _ := u.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
	workDir, _ := u.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory)

	contextArg, workDir := handleSetWorkingDirectory(setWorkDir, workDir, file, filepath.Dir(unitPath))
	named := contextArg != ""
	if !named && !strings.HasPrefix(file, "%") && !filepath.IsAbs(file) && !isQuadletURL(file) {
		// ConvertBuild passes the working directory as the context of a
		// relative File= (and of none).
		contextArg = workDir
	}
	if contextArg == "" {
		return withoutContext(file, workDir)
	}
	p := withContext(file, contextArg, workDir)
	p.NamedContext = named && p.Context != ""
	return p
}

// handleSetWorkingDirectory returns the context argument ConvertBuild passes
// to podman build for SetWorkingDirectory= ("" for none) and the build
// service's working directory ("" for systemd's default), following the
// Quadlet function of that name: an explicit [Service] WorkingDirectory= wins
// over the one SetWorkingDirectory= derives, and drops a unit-relative
// SetWorkingDirectory= path.
func handleSetWorkingDirectory(setWorkDir, workDir, file, unitDir string) (contextArg, serviceWorkDir string) {
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
	case filepath.IsAbs(setWorkDir) || isQuadletURL(setWorkDir):
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
	if file == "" || readsElsewhere(file) {
		return Paths{}
	}
	f, ok := absPath(file, workDir)
	if !ok {
		return Paths{}
	}
	return Paths{Context: filepath.Dir(f), Containerfiles: []string{f}, ContainerfilesComplete: true}
}

// withContext returns the paths of a build podman runs with the context
// argument contextArg.
func withContext(file, contextArg, workDir string) Paths {
	var p Paths
	if !readsElsewhere(contextArg) {
		p.Context, _ = absPath(contextArg, workDir)
	}
	if file == "" {
		// podman build looks for these in the context.
		if p.Context != "" {
			p.Containerfiles = []string{filepath.Join(p.Context, "Containerfile"), filepath.Join(p.Context, "Dockerfile")}
			p.ContainerfilesComplete = true
		}
		return p
	}
	if readsElsewhere(file) {
		return p
	}
	first, ok := absPath(file, workDir)
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

// absPath returns p as an absolute clean path, a relative one against the
// working directory; ok is false when it contains a systemd specifier or is
// relative to a working directory the unit does not set.
func absPath(p, workDir string) (string, bool) {
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

// isQuadletURL reports whether ConvertBuild treats a File= or
// SetWorkingDirectory= value as a URL (quadlet.URL, which also matches any
// value starting with "http").
func isQuadletURL(p string) bool {
	return quadlet.URL.MatchString(p)
}

// readsElsewhere reports whether podman build reads a context or
// Containerfile argument from somewhere other than a local path: a URL it
// fetches (cmd/podman/common/build.go isURL) or stdin ("-").
func readsElsewhere(p string) bool {
	if p == "-" {
		return true
	}
	for _, prefix := range []string{"http://", "https://", "git://", "github.com/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
