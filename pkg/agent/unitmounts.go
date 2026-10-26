package agent

import (
	"path"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

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
// type), Tmpfs= and Secret= (type=mount, the default), then the mounts its
// PodmanArgs= pass to podman run (-v/--volume, --mount, --tmpfs, --secret; a
// .volume source there is a volume name, not a unit). It returns apart the
// PodmanArgs= options that mount something the guard cannot read
// (--volumes-from, which mounts what another container mounts), each as
// "flag value". A flag is read wherever it appears, even as another flag's
// value, so a mount is never missed.
func unitMounts(unit *parser.UnitFile) (mounts []mount, opaque []string) {
	for _, volume := range unit.LookupAll("Container", "Volume") {
		mounts = append(mounts, parseVolume(volume))
	}
	for _, spec := range unit.LookupAllArgs("Container", "Mount") {
		if m, ok := parseMount(spec); ok {
			mounts = append(mounts, m)
		}
	}
	for _, tmpfs := range unit.LookupAll("Container", "Tmpfs") {
		mounts = append(mounts, parseTmpfs(tmpfs))
	}
	for _, secret := range unit.LookupAllArgs("Container", "Secret") {
		if m, ok := parseSecret(secret); ok {
			mounts = append(mounts, m)
		}
	}
	podmanArgsValues(unit, podmanMountFlags, func(flag, value string) {
		if flag == "volumes-from" {
			opaque = append(opaque, "--"+flag+" "+value)
		} else if m, ok := parsePodmanMount(flag, value); ok {
			mounts = append(mounts, m)
		}
	})
	return mounts, opaque
}

// unitSecret is a Podman secret unit's container uses, of either type
// (mounted or an environment variable): its name, and the setting that
// declares it (Secret=… or PodmanArgs=--secret …).
type unitSecret struct {
	name string
	decl string
}

// unitSecrets lists every Podman secret unit's [Container] group uses, of
// either type: its Secret= values, then the --secret values its PodmanArgs=
// pass to podman run. unitMounts reports only the mounted ones.
func unitSecrets(unit *parser.UnitFile) []unitSecret {
	var secrets []unitSecret
	add := func(spec, decl string) {
		if name, _, _ := resolver.ParseSecretMount(spec); name != "" {
			secrets = append(secrets, unitSecret{name: name, decl: decl})
		}
	}
	for _, spec := range unit.LookupAllArgs("Container", "Secret") {
		add(spec, "Secret="+spec)
	}
	podmanArgsValues(unit, podmanSecretFlags, func(_, value string) {
		add(value, "PodmanArgs=--secret "+value)
	})
	return secrets
}

// podmanArgsValues calls each, in order, with every flag of unit's
// PodmanArgs= that spec recognizes (podmanFlag) and its value (the next
// argument when the flag carries none; a flag at the end without one is
// dropped).
func podmanArgsValues(unit *parser.UnitFile, spec flagSpec, each func(flag, value string)) {
	args := unit.LookupAllArgs("Container", "PodmanArgs")
	for i := 0; i < len(args); i++ {
		flag, value, hasValue := podmanFlag(args[i], spec)
		if flag == "" {
			continue
		}
		if !hasValue {
			if i+1 == len(args) {
				break
			}
			i++
			value = args[i]
		}
		each(flag, value)
	}
}

// parsePodmanMount parses the value of the podman run mount flag flag (its
// long name, not volumes-from); ok is false when it mounts nothing.
func parsePodmanMount(flag, value string) (mount, bool) {
	switch flag {
	case "volume":
		return literalVolumeName(parseVolume(value)), true
	case "mount":
		m, ok := parseMount(value)
		return literalVolumeName(m), ok
	case "tmpfs":
		return parseTmpfs(value), true
	default:
		return parseSecret(value)
	}
}

// flagSpec names podman run flags to look for: their long names, and the
// one-letter flag short, whose long name is shortName.
type flagSpec struct {
	longs     []string
	short     rune
	shortName string
}

// podmanMountFlags are the podman run flags that mount something.
var podmanMountFlags = flagSpec{
	longs: []string{"volume", "mount", "tmpfs", "secret", "volumes-from"},
	short: 'v', shortName: "volume",
}

// podmanSecretFlags is podman run's secret flag, which has no short form.
var podmanSecretFlags = flagSpec{longs: []string{"secret"}}

// podmanWorkdirFlags is podman run's working-directory flag.
var podmanWorkdirFlags = flagSpec{longs: []string{"workdir"}, short: 'w', shortName: "workdir"}

// podmanValueShorthands are podman run's one-letter flags that take a value.
const podmanValueShorthands = "acehlmpuvw"

// podmanFlag reports which of spec's podman run flags arg starts, by its
// long name ("" for none), and the value it carries: --flag=value, -vvalue
// or -v=value, also after one-letter boolean flags (-dv). hasValue is false
// when the value is the next argument.
func podmanFlag(arg string, spec flagSpec) (flag, value string, hasValue bool) {
	if long, ok := strings.CutPrefix(arg, "--"); ok {
		name, value, hasValue := strings.Cut(long, "=")
		if !slices.Contains(spec.longs, name) {
			return "", "", false
		}
		return name, value, hasValue
	}
	cluster, ok := strings.CutPrefix(arg, "-")
	if !ok {
		return "", "", false
	}
	for i, c := range cluster {
		switch {
		case c == spec.short:
			value := strings.TrimPrefix(cluster[i+1:], "=")
			return spec.shortName, value, value != ""
		case strings.ContainsRune(podmanValueShorthands, c):
			return "", "", false
		}
	}
	return "", "", false
}

// workingDir returns the working directory unit's container starts in, as
// Quadlet passes it to podman run: WorkingDir= (--workdir), overridden by
// the last -w/--workdir its PodmanArgs= pass after it; set is false when
// the unit sets none and the image's applies.
func workingDir(unit *parser.UnitFile) (dir string, set bool) {
	dir, _ = unit.Lookup("Container", "WorkingDir")
	set = dir != "" // Quadlet skips an empty WorkingDir=
	podmanArgsValues(unit, podmanWorkdirFlags, func(_, value string) {
		dir, set = value, true
	})
	return dir, set
}

// literalVolumeName reads a .volume unit source as a volume name: podman run
// does not know Quadlet's units.
func literalVolumeName(m mount) mount {
	if m.source.kind == sourceVolumeUnit {
		m.source.kind = sourceVolume
	}
	return m
}

// parseVolume parses a Volume= (or -v) value: source:target[:options], or a
// target alone (an anonymous volume).
func parseVolume(volume string) mount {
	parts := strings.SplitN(volume, ":", 3)
	if len(parts) == 1 {
		return mount{target: path.Clean(parts[0])}
	}
	source := volumeSource(parts[0])
	if len(parts) == 3 && source.shared() {
		source.subpath, source.options = volumeOptions(strings.Split(parts[2], ","), volumeNeutralOptions)
	}
	return mount{source: source, target: path.Clean(parts[1])}
}

// parseTmpfs parses a Tmpfs= (or --tmpfs) value: target[:options].
func parseTmpfs(tmpfs string) mount {
	dir, _, _ := strings.Cut(tmpfs, ":")
	return mount{target: path.Clean(dir)}
}

// parseSecret parses a Secret= (or --secret) value; ok is false for an env
// secret.
func parseSecret(spec string) (mount, bool) {
	name, target, ok := resolver.ParseSecretMount(spec)
	if !ok {
		return mount{}, false
	}
	return mount{source: mountSource{kind: sourceSecret, name: name}, target: target}, true
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
