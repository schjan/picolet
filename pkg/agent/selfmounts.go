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

// credentialLocator returns the agentcfg.Locate for a config the Agent's next
// start reads with the mounts of next, checked by the Agent running in a
// container with the mounts of current; changed reports a source the
// changeset rewrites. A credential path under a mount of next is read
// through a mount of current with the same source, at its target, when no
// deeper mount of current shadows it there; one under no mount of next is
// inside the image and read where it is spelled, unless current mounts it:
// next drops that mount. A file credential the running container cannot
// see as the next start will is refused; a directory credential
// (protonpass.session_dir) need not exist, so it is read where it is
// spelled instead.
func credentialLocator(next, current []mount, changed func(mountSource) bool) agentcfg.Locate {
	return func(cred agentcfg.Credential) (string, error) {
		held, rel := mountsHolding(next, cred.Path)
		if len(held) == 0 {
			return inImage(cred, current)
		}
		m, err := single(held)
		if err != nil {
			return "", err
		}
		at, shadow := translate(m, rel, current)
		switch {
		case cred.Dir && at != "":
			return at, nil
		case cred.Dir:
			return cred.Path, nil
		case m.source.shared() && changed(m.source):
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
// names (of either type), or a Fleet-managed .volume unit a Volume= or
// Mount= names. Podman could not create the Agent's container without the
// secret, nor Quadlet generate its service without the .volume unit, so the
// Agent would not start again. It runs on every changeset, whether or not
// it writes the Agent's config or unit.
func checkLostSources(next *parser.UnitFile, unit string, changeset *reconciler.Changeset) error {
	for _, spec := range next.LookupAllArgs("Container", "Secret") {
		name, _, _ := resolver.ParseSecretMount(spec)
		if name != "" && changeset.Deletes(resolver.SecretDestPath(name)) {
			return fmt.Errorf("refusing to remove the secret %s: the Agent unit %s still mounts it (Secret=%s), so the Agent could not start; "+
				"keep resolving the secret, or stop mounting it", name, unit, spec)
		}
	}
	for _, m := range unitMounts(next) {
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
// manage), and then credentials are read where they are spelled.
func selfConfigLocator(next, running *parser.UnitFile, changeset *reconciler.Changeset) agentcfg.Locate {
	if next == nil || running == nil {
		return nil
	}
	locate := credentialLocator(unitMounts(next), unitMounts(running), sourceChanges(changeset))
	if opaque := slices.Concat(opaqueMounts(next), opaqueMounts(running)); len(opaque) > 0 {
		return refuseOpaque(opaque, locate)
	}
	return locate
}

// refuseOpaque wraps locate to refuse every file credential: opaque lists
// mounts of the next or running unit the guard cannot read (opaqueMounts),
// so it cannot tell what the next start sees at any path.
func refuseOpaque(opaque []string, locate agentcfg.Locate) agentcfg.Locate {
	return func(cred agentcfg.Credential) (string, error) {
		if cred.Dir {
			return locate(cred)
		}
		return "", fmt.Errorf("the Agent unit passes %s in PodmanArgs=, which mounts what the guard cannot follow, so it cannot tell what the next start sees at %s; "+
			"declare the Agent's mounts with Volume=, Mount=, Tmpfs= or Secret=", strings.Join(opaque, ", "), cred.Path)
	}
}
