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

// sourceKind is what a mount of the Agent quadlet's container mounts.
type sourceKind int

const (
	// sourceNone is a mount no other container shares: an anonymous
	// volume, or a tmpfs, ramfs or devpts mount.
	sourceNone sourceKind = iota
	// sourceVolume is a Volume= or Mount= bind or volume source: a host
	// path or a volume name.
	sourceVolume
	// sourceVolumeUnit is a Volume= or Mount= source naming a .volume unit
	// (the Fleet may manage it).
	sourceVolumeUnit
	// sourceSecret is a Podman secret, by name.
	sourceSecret
	// sourceSpec is any other Mount= type (glob, image, …), named by its
	// spec without the destination.
	sourceSpec
)

// mountSource is what a mount mounts. Sources are compared as written, so the
// same specifier (%h) in two units matches; two mounts with equal sources
// show the same files. For a bind or volume source, subpath is the
// subdirectory it mounts (cleaned, relative) and options the sorted options
// that may change what it shows (contentOptions).
type mountSource struct {
	kind    sourceKind
	name    string
	subpath string
	options string
}

// shared reports whether the source is one the running container can share.
func (s mountSource) shared() bool { return s.kind != sourceNone }

func (s mountSource) String() string {
	switch s.kind {
	case sourceNone:
		return "an anonymous volume or tmpfs"
	case sourceSecret:
		return "secret " + s.name
	case sourceSpec:
		return "Mount=" + s.name
	case sourceVolume, sourceVolumeUnit:
	}
	var details []string
	if s.subpath != "" {
		details = append(details, "subpath "+s.subpath)
	}
	if s.options != "" {
		details = append(details, "options "+s.options)
	}
	if len(details) == 0 {
		return s.name
	}
	return s.name + " (" + strings.Join(details, ", ") + ")"
}

// mount is one mount of the Agent quadlet's container: what it mounts
// (source) and where (target, inside the container).
type mount struct {
	source mountSource
	target string
}

// unitMounts lists the mounts unit's [Container] group declares, read as
// Quadlet reads them: Volume= (source:target[:options]), Mount= (every
// type), Tmpfs= and Secret= (type=mount, the default).
func unitMounts(unit *parser.UnitFile) []mount {
	var mounts []mount
	for _, volume := range unit.LookupAll("Container", "Volume") {
		parts := strings.SplitN(volume, ":", 3)
		if len(parts) == 1 {
			mounts = append(mounts, mount{target: path.Clean(parts[0])})
			continue
		}
		source := volumeSource(parts[0])
		if len(parts) == 3 && source.shared() {
			source.subpath, source.options = volumeOptions(strings.Split(parts[2], ","), volumeNeutralOptions)
		}
		mounts = append(mounts, mount{source: source, target: path.Clean(parts[1])})
	}
	for _, spec := range unit.LookupAllArgs("Container", "Mount") {
		if m, ok := parseMount(spec); ok {
			mounts = append(mounts, m)
		}
	}
	for _, tmpfs := range unit.LookupAll("Container", "Tmpfs") {
		dir, _, _ := strings.Cut(tmpfs, ":")
		mounts = append(mounts, mount{target: path.Clean(dir)})
	}
	for _, secret := range unit.LookupAllArgs("Container", "Secret") {
		if name, target, ok := resolver.ParseSecretMount(secret); ok {
			mounts = append(mounts, mount{source: mountSource{kind: sourceSecret, name: name}, target: target})
		}
	}
	return mounts
}

// volumeSource is the source of a bind or volume mount; a host path is
// cleaned and a volume name kept; "" is an anonymous volume.
func volumeSource(source string) mountSource {
	switch {
	case source == "":
		return mountSource{}
	case strings.HasPrefix(source, "/"):
		return mountSource{kind: sourceVolume, name: path.Clean(source)}
	case strings.HasSuffix(source, ".volume"):
		return mountSource{kind: sourceVolumeUnit, name: source}
	default:
		return mountSource{kind: sourceVolume, name: source}
	}
}

// volumeNeutralOptions are the Volume= options that change only access,
// labelling or propagation, never which files a mount shows or who may read
// them.
var volumeNeutralOptions = []string{
	"ro", "rw", "z", "Z",
	"exec", "noexec", "suid", "nosuid", "dev", "nodev",
	"private", "rprivate", "shared", "rshared", "slave", "rslave", "unbindable", "runbindable",
}

// mountNeutralKeys are the Mount= keys that change only access, labelling or
// propagation (see volumeNeutralOptions).
var mountNeutralKeys = []string{"ro", "readonly", "rw", "relabel", "bind-propagation", "z", "Z"}

// volumeOptions splits a bind or volume mount's options into its subpath
// (subpath= or volume-subpath=, cleaned and relative to the source) and the
// sorted rest, minus those whose key is in neutral.
func volumeOptions(options, neutral []string) (subpath, rest string) {
	var kept []string
	for _, option := range options {
		key, value, _ := strings.Cut(option, "=")
		switch {
		case option == "":
		case key == "subpath" || key == "volume-subpath":
			subpath = strings.TrimPrefix(path.Clean("/"+value), "/")
		case !slices.Contains(neutral, key):
			kept = append(kept, option)
		}
	}
	slices.Sort(kept)
	return subpath, strings.Join(kept, ",")
}

// parseMount parses a Mount= value (type=…,source=…,destination=…). A bind
// or volume mount is named by its source, subpath and options, a tmpfs,
// ramfs or devpts mount shares nothing, and any other type is named by its
// spec without the destination. ok is false without a destination.
func parseMount(spec string) (mount, bool) {
	fields := mountFields(spec)
	if fields.target == "" {
		return mount{}, false
	}
	switch fields.kind {
	case "bind", "volume":
		s := volumeSource(fields.source)
		if s.shared() {
			s.subpath, s.options = volumeOptions(fields.options, mountNeutralKeys)
		}
		return mount{source: s, target: fields.target}, true
	case "tmpfs", "ramfs", "devpts":
		return mount{target: fields.target}, true
	default:
		return mount{source: mountSource{kind: sourceSpec, name: fields.rest}, target: fields.target}, true
	}
}

// mountSpec is a Mount= value's fields: rest is the spec without the
// destination, options its fields other than type, source and destination.
type mountSpec struct {
	kind, source, target, rest string
	options                    []string
}

func mountFields(spec string) mountSpec {
	var fields mountSpec
	var rest []string
	for field := range strings.SplitSeq(spec, ",") {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "type":
			fields.kind = value
		case "source", "src":
			fields.source = value
		case "destination", "dst", "target":
			fields.target = path.Clean(value)
			continue
		default:
			fields.options = append(fields.options, field)
		}
		rest = append(rest, field)
	}
	fields.rest = strings.Join(rest, ",")
	return fields
}

// mountsHolding returns the mounts whose target holds p, the deepest ones
// (the container sees the innermost mount; more than one has the same
// target), and p relative to that target.
func mountsHolding(mounts []mount, p string) (held []mount, rel string) {
	p = path.Clean(p)
	for _, candidate := range mounts {
		r, under := relativeTo(candidate.target, p)
		switch {
		case !under:
		case len(held) == 0 || len(candidate.target) > len(held[0].target):
			held, rel = []mount{candidate}, r
		case candidate.target == held[0].target:
			held = append(held, candidate)
		}
	}
	return held, rel
}

// mountHolding is mountsHolding for a container that has started: when more
// than one mount has the deepest target, it reports the first, and ok is
// false when no mount holds p.
func mountHolding(mounts []mount, p string) (m mount, rel string, ok bool) {
	held, rel := mountsHolding(mounts, p)
	if len(held) == 0 {
		return mount{}, "", false
	}
	return held[0], rel, true
}

// relativeTo returns p relative to dir when p is dir or under it.
func relativeTo(dir, p string) (string, bool) {
	if p == dir {
		return ".", true
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	if rest, ok := strings.CutPrefix(p, prefix); ok {
		return rest, true
	}
	return "", false
}

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
	return credentialLocator(unitMounts(next), unitMounts(running), sourceChanges(changeset))
}
