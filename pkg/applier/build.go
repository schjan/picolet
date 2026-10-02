package applier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/buildunit"
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

// WithHostDataDir names the host-visible path (hostDataDir) of the data
// directory picolet writes files and manifests to (dataDir), both from
// resolver.ResolvedHost. A .build names its Containerfile and build context
// by host path; without this option the two are the same, and a containerized
// Agent would never rebuild on a changed input.
func WithHostDataDir(dataDir, hostDataDir string) Option {
	return func(a *Applier) {
		a.dataDir, a.hostDataDir = dataDir, hostDataDir
	}
}

// hostPath returns the path the Host sees for a file picolet writes at path.
func (a *Applier) hostPath(path string) string {
	if a.dataDir == a.hostDataDir || !isWithin(path, a.dataDir) {
		return path
	}
	rel, _ := filepath.Rel(a.dataDir, path)
	return filepath.Join(a.hostDataDir, rel)
}

// triggeredBuilds returns the services of the RestartRebuild units (.build)
// that must run, sorted, and the image tags they write (ImageTag=): those
// whose own file was created or updated, and those one of whose local inputs
// (buildunit.Resolve: Containerfile, anything under the build context) was
// created, updated or deleted. Podman generates no trigger for either; a
// build otherwise runs only when a consumer next starts.
func (a *Applier) triggeredBuilds(changes []reconciler.Change) (builds, tags []string) {
	var changed []string
	for _, c := range changes {
		if c.Action != reconciler.ActionNoop && destination(c.Category) != config.DestSecret {
			changed = append(changed, a.hostPath(c.DestPath))
		}
	}
	for _, c := range changes {
		spec, _ := config.SpecFor(c.Category)
		if spec.Restart == config.RestartRebuild && c.ServiceName != "" && buildTriggered(c, changed) {
			builds = append(builds, c.ServiceName)
			if u := parseUnitFile(filepath.Base(c.DestPath), c.NewContent); u != nil {
				tags = append(tags, u.LookupAll(quadlet.BuildGroup, quadlet.KeyImageTag)...)
			}
		}
	}
	slices.Sort(builds)
	slices.Sort(tags)
	return slices.Compact(builds), slices.Compact(tags)
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
	u := parseUnitFile(filepath.Base(c.DestPath), c.NewContent)
	if u == nil {
		return false
	}
	inputs := buildunit.Resolve(u, c.DestPath)
	return slices.ContainsFunc(changed, func(p string) bool {
		return slices.Contains(inputs.Containerfiles, p) || isWithin(p, inputs.Context)
	})
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
// build has succeeded. The first failure fails the apply before anything is
// restarted, and every tag is put back on the image it named before, so no
// image built from inputs the rollback restores (an earlier build of the same
// apply) stays tagged.
func (a *Applier) runBuilds(ctx context.Context, builds, tags []string, result *ApplyResult) error {
	if len(builds) == 0 {
		return nil
	}
	before, err := a.imageIDs(ctx, tags)
	if err != nil {
		return fmt.Errorf("recording image tags before rebuilding: %w", err)
	}
	for _, unit := range builds {
		slog.Info("building image", "unit", unit)
		if err := a.systemd.RunBuildUnit(ctx, unit); err != nil {
			return errors.Join(fmt.Errorf("rebuilding image: %w", err), a.restoreTags(ctx, tags, before))
		}
		result.RestartedUnits = append(result.RestartedUnits, unit)
	}
	return nil
}

// imageIDs returns the image ID each tag names ("" for none).
func (a *Applier) imageIDs(ctx context.Context, tags []string) (map[string]string, error) {
	ids := make(map[string]string, len(tags))
	for _, tag := range tags {
		id, err := a.podman.ImageID(ctx, tag)
		if err != nil {
			return nil, err
		}
		ids[tag] = id
	}
	return ids, nil
}

// restoreTags points every tag back at the image it named before (untagging
// one that named none). Uses a context detached from cancellation: it runs
// on the failure path, also when the apply was cancelled.
func (a *Applier) restoreTags(ctx context.Context, tags []string, before map[string]string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobTimeout)
	defer cancel()
	var errs []error
	for _, tag := range tags {
		now, err := a.podman.ImageID(ctx, tag)
		switch {
		case err != nil:
			errs = append(errs, err)
		case now == before[tag]:
		case before[tag] == "":
			slog.Warn("removing tag of a build the failed apply rolls back", "tag", tag)
			errs = append(errs, a.podman.ImageUntag(ctx, tag))
		default:
			slog.Warn("restoring tag of a build the failed apply rolls back", "tag", tag, "image", before[tag])
			errs = append(errs, a.podman.ImageTag(ctx, before[tag], tag))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("restoring image tags: %w", err)
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

// restartConsumer restarts a running consumer of a rebuilt image without its
// dependencies, so the build service is not started again and cannot take the
// consumer down a second time. A consumer that is not running is started the
// ordinary way, with its dependencies (and so the build again, from cache):
// nothing runs that it could take down. A one-shot systemd activates (a
// timer's job) is left to its trigger. A failure is a failed restart like any
// other (pending).
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
	slog.Info("restarting consumer of rebuilt image", "unit", unit, "active_state", st.ActiveState)
	switch st.ActiveState {
	case "active", "activating", "reloading":
		recordRestart(unit, a.systemd.RestartUnitIgnoringDependencies(ctx, unit), result)
	default:
		recordRestart(unit, a.systemd.RestartUnit(ctx, unit), result)
	}
}

// scheduleSelfConsumerRestarts defers the restart of the agent's own units
// among the consumers, like any self restart, but without dependencies: an
// ordinary restart would run the build again under the agent.
func (a *Applier) scheduleSelfConsumerRestarts(units []string, result *ApplyResult) {
	for _, unit := range units {
		slog.Info("restarting picolet (rebuilt image), state will be saved before shutdown", "unit", unit)
		result.RestartedUnits = append(result.RestartedUnits, unit)
		a.deferredSelfUnitOp(unit, a.systemd.RestartUnitIgnoringDependencies)
	}
}
