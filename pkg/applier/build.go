package applier

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/status"
)

// WithDependencies supplies the generated unit dependencies validation
// computed (validator.AnalyzeFiles). The consumers of a rebuilt image are the
// units that Require= its build service, and a pod's Wants= names the
// members its restart starts; without this option a rebuild restarts no
// consumer.
func WithDependencies(deps map[string]status.UnitDependencies) Option {
	return func(a *Applier) {
		a.deps = deps
	}
}

// triggeredBuilds returns the services of the RestartRebuild units (.build)
// that must run, sorted: those whose own file was created or updated, and
// those one of whose local inputs (buildInputs) was created, updated or
// deleted. Podman generates no trigger for either; a build otherwise runs
// only when a consumer next starts.
func triggeredBuilds(changes []reconciler.Change) []string {
	var changed []string
	for _, c := range changes {
		if c.Action != reconciler.ActionNoop && destination(c.Category) != config.DestSecret {
			changed = append(changed, c.DestPath)
		}
	}
	var builds []string
	for _, c := range changes {
		spec, _ := config.SpecFor(c.Category)
		if spec.Restart == config.RestartRebuild && c.ServiceName != "" && buildTriggered(c, changed) {
			builds = append(builds, c.ServiceName)
		}
	}
	slices.Sort(builds)
	return slices.Compact(builds)
}

// buildTriggered reports whether the .build change c must run: its file was
// created or updated, or it is unchanged and one of its inputs is in changed.
func buildTriggered(c reconciler.Change, changed []string) bool {
	switch c.Action {
	case reconciler.ActionCreate, reconciler.ActionUpdate:
		return true
	case reconciler.ActionDelete:
		return false
	case reconciler.ActionNoop:
	}
	file, contextDir := buildInputs(parseUnitFile(filepath.Base(c.DestPath), c.NewContent), c.DestPath)
	return slices.ContainsFunc(changed, func(p string) bool {
		return p == file || isWithin(p, contextDir)
	})
}

// buildInputs returns the local paths a .build unit at unitPath builds from:
// its Containerfile (File=) and its build context directory, "" for either
// when it is remote (a URL), starts with a systemd specifier, or is unset.
// A relative File= is relative to the unit's directory for
// SetWorkingDirectory=file without a [Service] WorkingDirectory=, to the
// build service's working directory otherwise (buildDirs).
func buildInputs(u *parser.UnitFile, unitPath string) (file, contextDir string) {
	if u == nil {
		return "", ""
	}
	file, _ = u.Lookup(quadlet.BuildGroup, quadlet.KeyFile)
	setWorkDir, _ := u.Lookup(quadlet.BuildGroup, quadlet.KeySetWorkingDirectory)
	serviceWorkDir, _ := u.Lookup(quadlet.ServiceGroup, quadlet.ServiceKeyWorkingDirectory)
	unitDir := filepath.Dir(unitPath)
	if strings.EqualFold(setWorkDir, "file") && serviceWorkDir == "" && isRelative(file) {
		file = filepath.Join(unitDir, file)
	}
	workDir, contextDir := buildDirs(serviceWorkDir, setWorkDir, file, unitDir)
	if isRelative(file) && workDir != "" {
		file = filepath.Join(workDir, file)
	}
	return localPath(file), localPath(contextDir)
}

// buildDirs returns a .build service's working directory and build context,
// following Quadlet's ConvertBuild (handleSetWorkingDirectory). workDir is
// the [Service] WorkingDirectory=; when set, it is the context unless
// SetWorkingDirectory= names an absolute path or URL. Otherwise
// SetWorkingDirectory=file and =unit make the Containerfile's or the unit's
// directory the working directory and so the context, and a relative path is
// the context relative to the unit's directory. Without either key there is
// no context: only File= counts.
func buildDirs(workDir, setWorkDir, file, unitDir string) (string, string) {
	switch strings.ToLower(setWorkDir) {
	case "":
		return workDir, workDir
	case "file":
		if workDir == "" && file != "" {
			workDir = filepath.Dir(file)
		}
		return workDir, workDir
	case "unit":
		if workDir == "" {
			workDir = unitDir
		}
		return workDir, workDir
	}
	if !isRelative(setWorkDir) {
		return workDir, setWorkDir
	}
	if workDir != "" {
		return workDir, workDir
	}
	return unitDir, filepath.Join(unitDir, setWorkDir)
}

// isRelative reports whether p is a relative local path: not absolute, not a
// URL, not starting with a systemd specifier (which Quadlet leaves alone).
func isRelative(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !isRemote(p) && !strings.HasPrefix(p, "%")
}

// localPath returns p cleaned when it is an absolute local path, "" otherwise.
func localPath(p string) string {
	if !filepath.IsAbs(p) || isRemote(p) {
		return ""
	}
	return filepath.Clean(p)
}

// isRemote reports whether a Quadlet build source is a URL (Quadlet's isURL).
func isRemote(p string) bool {
	return strings.Contains(p, "://") || strings.HasPrefix(p, "github.com/")
}

// isWithin reports whether path lies strictly inside dir.
func isWithin(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

// runBuilds starts each triggered build and waits for it. A start, never a
// restart: systemd propagates a restart of a Requires= dependency to the
// units requiring it, which would stop the running consumers before the
// build has succeeded. The first failure fails the apply — nothing has been
// restarted yet, and the previously built image is still tagged.
func (a *Applier) runBuilds(ctx context.Context, builds []string, result *ApplyResult) error {
	for _, unit := range builds {
		slog.Info("building image", "unit", unit)
		if err := a.systemd.RunBuildUnit(ctx, unit); err != nil {
			return fmt.Errorf("rebuilding image: %w", err)
		}
		result.RestartedUnits = append(result.RestartedUnits, unit)
	}
	return nil
}

// takeBuildConsumers returns the units requiring a rebuilt build service,
// sorted, and removes them from the ordinary restart set: an ordinary
// (re)start starts the inactive one-shot build service again, so the build
// would run twice. Members a restarting pod starts (its Wants=) are left to
// the pod.
func (a *Applier) takeBuildConsumers(p *applyPhaseResult) []string {
	var podStarted []string
	for _, pod := range p.ChangedPods {
		podStarted = append(podStarted, a.deps[pod].Wants...)
	}
	consumers := make(map[string]struct{})
	for unit, deps := range a.deps {
		if slices.ContainsFunc(deps.Requires, func(dep string) bool { return slices.Contains(p.Builds, dep) }) &&
			!slices.Contains(podStarted, unit) {
			consumers[unit] = struct{}{}
			delete(p.ChangedUnits, unit)
		}
	}
	return slices.Sorted(maps.Keys(consumers))
}

// restartConsumer restarts a consumer of a rebuilt image without its
// dependencies, so the build service is not started again; an inactive
// consumer is started the same way. Runs after the ordinary restarts, so
// dependencies changed alongside it are already up; if one is not, the start
// fails and the health loop restarts the failed consumer the ordinary way. A
// one-shot systemd activates (a timer's job) is left to its trigger. A
// failure is a failed restart like any other (pending).
func (a *Applier) restartConsumer(ctx context.Context, unit string, result *ApplyResult) {
	st, err := a.systemd.GetUnitStatus(ctx, unit)
	if err != nil {
		recordRestart(unit, fmt.Errorf("checking %s before restart: %w", unit, err), result)
		return
	}
	if ExternallyActivated(st) {
		slog.Info("skipping restart of externally activated consumer", "unit", unit)
		result.SystemdUnitOps = append(result.SystemdUnitOps,
			SystemdUnitOp{Unit: unit, Operation: SystemdOpRestart, Result: SystemdOpResultSkipped})
		return
	}
	slog.Info("restarting consumer of rebuilt image", "unit", unit)
	recordRestart(unit, a.systemd.RestartUnitIgnoringDependencies(ctx, unit), result)
}
