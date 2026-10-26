package agent

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// errMountChange ends every refusal of a credential the running container
// cannot check.
const errMountChange = "land the mount change in its own commit first, and point the config at the mount once the Agent runs with it"

// selfLocator locates the credentials of the Agent config the Agent's next
// start reads with the mounts of next, checked by the Agent running in a
// container with the mounts of current (selfConfigLocator).
type selfLocator struct {
	next, current []mount
	// changed reports a source the changeset rewrites.
	changed func(mountSource) bool
	// opaque lists mounts of the next or running unit the guard cannot read
	// (unitMounts).
	opaque []string
	// resolveRelative resolves a relative credential path against dir, the
	// next start's working directory; dirErr, when dir is unknown, refuses
	// every relative credential path.
	resolveRelative bool
	dir             string
	dirErr          error
}

// locate is the agentcfg.Locate of l: it resolves a relative path
// (resolveRelative), refuses every file credential when a mount is opaque
// (the guard cannot tell what the next start sees at any path), then
// locates the path through the mounts (throughMounts).
func (l selfLocator) locate(cred agentcfg.Credential) (string, error) {
	if l.resolveRelative && !path.IsAbs(cred.Path) {
		if l.dirErr != nil {
			return "", fmt.Errorf("the path is relative and this commit changes the Agent unit, but %w; make the path absolute", l.dirErr)
		}
		cred.Path = path.Join(l.dir, cred.Path)
	}
	if len(l.opaque) > 0 && !cred.Dir {
		return "", fmt.Errorf("the Agent unit passes %s in PodmanArgs=, which mounts what the guard cannot follow, so it cannot tell what the next start sees at %s; "+
			"declare the Agent's mounts with Volume=, Mount=, Tmpfs= or Secret=", strings.Join(l.opaque, ", "), cred.Path)
	}
	return l.throughMounts(cred)
}

// throughMounts locates cred through the mounts. A credential path under a
// mount of next is read through a mount of current with the same source, at
// its target, when no deeper mount of current shadows it there; one under no
// mount of next is inside the image and read where it is spelled, unless
// current mounts it: next drops that mount. A file credential the running
// container cannot see as the next start will is refused; a directory
// credential (protonpass.session_dir) need not exist, so it is read where it
// is spelled instead.
func (l selfLocator) throughMounts(cred agentcfg.Credential) (string, error) {
	held, rel := mountsHolding(l.next, cred.Path)
	if len(held) == 0 {
		return inImage(cred, l.current)
	}
	m, err := single(held)
	if err != nil {
		return "", err
	}
	at, shadow := translate(m, rel, l.current)
	switch {
	case cred.Dir && at != "":
		return at, nil
	case cred.Dir:
		return cred.Path, nil
	case m.source.shared() && l.changed(m.source):
		return "", fmt.Errorf("the new Agent unit mounts %s from %s, which this commit changes, so the running Agent's container shows its old content; %s",
			m.target, m.source, errMountChange)
	case at != "":
		return at, nil
	case shadow != nil:
		return "", fmt.Errorf("the new Agent unit mounts %s from %s, which the running Agent's container shadows at %s with %s; %s",
			m.target, m.source, shadow.target, shadow.source, errMountChange)
	}
	return "", fmt.Errorf("the new Agent unit mounts %s from %s, which the running Agent's container does not mount there; %s",
		m.target, m.source, errMountChange)
}

// single returns the one mount the next start shows at the deepest target
// held lists, and refuses two different sources there.
func single(held []mount) (mount, error) {
	m := held[0]
	for _, other := range held[1:] {
		if other.source != m.source {
			return mount{}, fmt.Errorf("the new Agent unit mounts both %s and %s at %s; %s", m.source, other.source, m.target, errMountChange)
		}
	}
	return m, nil
}

// inImage locates a credential the next start reads from its image (under no
// mount): where it is spelled, unless the running container, with the
// mounts of current, mounts it, and then a file credential is refused.
func inImage(cred agentcfg.Credential, current []mount) (string, error) {
	if c, _, ok := mountHolding(current, cred.Path); ok && !cred.Dir {
		return "", fmt.Errorf("the running Agent's container mounts %s from %s, which the new Agent unit no longer mounts; %s",
			c.target, c.source, errMountChange)
	}
	return cred.Path, nil
}

// translate returns where the running container, with the mounts of
// current, shows rel under the next start's mount m: under a mount of
// current with m's source, unless a deeper mount of current shadows that
// path. at is "" when there is no such place; shadow is then the mount of
// current that shadows it, if one does.
func translate(m mount, rel string, current []mount) (at string, shadow *mount) {
	if !m.source.shared() {
		return "", nil
	}
	for _, c := range current {
		if c.source != m.source {
			continue
		}
		p := path.Join(c.target, rel)
		held, _ := mountsHolding(current, p)
		if slices.Contains(held, c) {
			return p, nil
		}
		shadow = &held[0]
	}
	return "", shadow
}

// sourceChanges returns whether the changeset creates, updates or deletes a
// source a credential's mount comes from: a Podman secret, or a .volume unit
// a Volume= or Mount= names.
func sourceChanges(changeset *reconciler.Changeset) func(mountSource) bool {
	return func(source mountSource) bool {
		switch source.kind {
		case sourceSecret:
			dest := resolver.SecretDestPath(source.name)
			return changeset.Writes(dest) || changeset.Deletes(dest)
		case sourceVolumeUnit:
			for _, change := range volumeUnitChanges(changeset, source.name) {
				if change.Action != reconciler.ActionNoop {
					return true
				}
			}
		case sourceNone, sourceVolume, sourceSpec:
		}
		return false
	}
}

// volumeUnitChanges returns the changes to the .volume unit named name.
func volumeUnitChanges(changeset *reconciler.Changeset, name string) []reconciler.Change {
	var changes []reconciler.Change
	for _, change := range changeset.Changes {
		if change.Category == config.CategoryVolume && path.Base(change.DestPath) == name {
			changes = append(changes, change)
		}
	}
	return changes
}

// removesVolumeUnit reports whether the changeset deletes the .volume unit
// named name and writes none in its place.
func removesVolumeUnit(changeset *reconciler.Changeset, name string) bool {
	changes := volumeUnitChanges(changeset, name)
	return len(changes) > 0 && !slices.ContainsFunc(changes, func(c reconciler.Change) bool { return c.Action != reconciler.ActionDelete })
}

// checkLostSources refuses a changeset that removes a source the Agent's next
// start, with the unit next, needs: a Fleet-managed Podman secret a Secret=
// or a PodmanArgs= --secret names (of either type), or a Fleet-managed
// .volume unit a Volume= or Mount= names. Podman could not create the Agent's container without the
// secret, nor Quadlet generate its service without the .volume unit, so the
// Agent would not start again. It runs on every changeset, whether or not
// it writes the Agent's config or unit.
func checkLostSources(next *parser.UnitFile, unit string, changeset *reconciler.Changeset) error {
	for _, secret := range unitSecrets(next) {
		if changeset.Deletes(resolver.SecretDestPath(secret.name)) {
			return fmt.Errorf("refusing to remove the secret %s: the Agent unit %s still mounts it (%s), so the Agent could not start; "+
				"keep resolving the secret, or stop mounting it", secret.name, unit, secret.decl)
		}
	}
	mounts, _ := unitMounts(next)
	for _, m := range mounts {
		if m.source.kind == sourceVolumeUnit && removesVolumeUnit(changeset, m.source.name) {
			return fmt.Errorf("refusing to remove the volume unit %s: the Agent unit %s still mounts it at %s, so the Agent could not start; "+
				"keep the volume unit, or stop mounting it", m.source.name, unit, m.target)
		}
	}
	return nil
}

// selfConfigLocator returns how to read the credentials of the Agent config
// the Agent's next start reads, with the mounts of the Agent's quadlet next
// (resolved). The running Agent has the mounts of running, the unit on disk;
// nil when there is none (the Agent runs from a unit the Fleet does not
// manage), and then credentials are read where they are spelled. When the
// changeset writes the unit (writesUnit), a relative credential path is
// first resolved against the next start's working directory
// (nextWorkingDir, with getwd the running Agent's); otherwise both starts
// share the running Agent's, and it is read where it is spelled.
func selfConfigLocator(next, running *parser.UnitFile, writesUnit bool, changeset *reconciler.Changeset, getwd func() (string, error)) agentcfg.Locate {
	if next == nil || running == nil {
		return nil
	}
	nextMounts, nextOpaque := unitMounts(next)
	runningMounts, runningOpaque := unitMounts(running)
	l := selfLocator{
		next:            nextMounts,
		current:         runningMounts,
		changed:         sourceChanges(changeset),
		opaque:          slices.Concat(nextOpaque, runningOpaque),
		resolveRelative: writesUnit,
	}
	if writesUnit {
		l.dir, l.dirErr = nextWorkingDir(next, running, getwd)
	}
	return l.locate
}

// nextWorkingDir returns the working directory the Agent's next start, with
// the unit next, resolves relative paths against: next's own (workingDir),
// which must be an absolute path without specifiers; or, when next sets
// none, the image's, which is the running Agent's (getwd) only when running
// sets none either. An error says why it is unknown.
func nextWorkingDir(next, running *parser.UnitFile, getwd func() (string, error)) (string, error) {
	if dir, set := workingDir(next); set {
		if !path.IsAbs(dir) || strings.Contains(dir, "%") {
			return "", fmt.Errorf("the new Agent unit's working directory %q is not an absolute path the guard can read", dir)
		}
		return path.Clean(dir), nil
	}
	if dir, set := workingDir(running); set {
		return "", fmt.Errorf("the new Agent unit sets no working directory, so its next start uses the image's, while the running Agent's unit sets %q", dir)
	}
	dir, err := getwd()
	if err != nil {
		return "", fmt.Errorf("the running Agent's working directory is unknown: %w", err)
	}
	return dir, nil
}
