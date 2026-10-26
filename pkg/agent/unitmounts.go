package agent

import (
	"path"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/resolver"
)

// mountKeys are the Agent unit's [Container] keys that decide its mounts and
// working directory (sameMountLayout).
var mountKeys = []string{"Volume", "Mount", "Tmpfs", "Secret", "Mask", "Unmask", "VolatileTmp", "PodmanArgs", "WorkingDir"}

// sameMountLayout reports whether two Agent units spell their mountKeys the
// same: the guard compares those keys' values, not what they mean.
func sameMountLayout(a, b *parser.UnitFile) bool {
	for _, key := range mountKeys {
		if !slices.Equal(a.LookupAll("Container", key), b.LookupAll("Container", key)) {
			return false
		}
	}
	return true
}

// mount is one mount of the Agent unit's container: its target inside the
// container and its source as written, without the target. A tmpfs or
// anonymous volume has no source: it shows nothing another start shares.
type mount struct {
	source, target string
	// hostPath reports a bind (or Volume=) of a Host path: absolute,
	// relative to the unit's directory, or specifier-led (%h).
	hostPath bool
	// volumeUnit names the .volume unit a Volume= or Mount= mounts.
	volumeUnit string
	// secret names the Podman secret a Secret= or --secret mounts.
	secret string
}

// unitMounts lists the mounts unit's [Container] group declares, read as
// Quadlet reads them: Volume=, Mount=, Tmpfs= and Secret= (type=mount),
// the Mask=, Unmask= and VolatileTmp= paths (maskMounts), then the mounts its PodmanArgs= pass to podman run (-v/--volume, --mount,
// --tmpfs, --secret; a .volume source there is a volume name, not a unit).
// It returns apart the PodmanArgs= options that mount something the guard
// cannot read (--volumes-from), each as "flag value". A flag is read wherever
// it appears, even as another flag's value, so a mount is never missed.
func unitMounts(unit *parser.UnitFile) (mounts []mount, opaque []string) {
	for _, volume := range unit.LookupAll("Container", "Volume") {
		mounts = append(mounts, parseVolume(volume, true))
	}
	for _, spec := range unit.LookupAllArgs("Container", "Mount") {
		mounts = append(mounts, parseMount(spec, true))
	}
	for _, tmpfs := range unit.LookupAll("Container", "Tmpfs") {
		mounts = append(mounts, parseTmpfs(tmpfs))
	}
	for _, spec := range unit.LookupAllArgs("Container", "Secret") {
		mounts = appendSecret(mounts, spec)
	}
	mounts = append(mounts, maskMounts(unit)...)
	podmanArgsValues(unit, podmanMountFlags, func(flag, value string) {
		switch flag {
		case "volume":
			mounts = append(mounts, parseVolume(value, false))
		case "mount":
			mounts = append(mounts, parseMount(value, false))
		case "tmpfs":
			mounts = append(mounts, parseTmpfs(value))
		case "secret":
			mounts = appendSecret(mounts, value)
		default:
			opaque = append(opaque, "--"+flag+" "+value)
		}
	})
	return mounts, opaque
}

// maskMounts returns unit's Mask= and Unmask= paths, each as a mount whose
// source is "mask" or "unmask" (Unmask=ALL has no placed target), and
// VolatileTmp='s tmpfs at /tmp.
func maskMounts(unit *parser.UnitFile) []mount {
	var mounts []mount
	for _, key := range []string{"Mask", "Unmask"} {
		for _, paths := range unit.LookupAllArgs("Container", key) {
			for p := range strings.SplitSeq(paths, ":") {
				mounts = append(mounts, mount{source: strings.ToLower(key), target: path.Clean(p)})
			}
		}
	}
	if unit.LookupBooleanWithDefault("Container", "VolatileTmp", false) {
		mounts = append(mounts, mount{target: "/tmp"})
	}
	return mounts
}

// parseVolume parses a Volume= (or -v, quadlet false) value:
// source:target[:options], or a target alone (an anonymous volume).
func parseVolume(volume string, quadlet bool) mount {
	parts := strings.SplitN(volume, ":", 3)
	if len(parts) == 1 || parts[0] == "" {
		return mount{target: path.Clean(parts[len(parts)-1])}
	}
	m := mount{source: parts[0], target: path.Clean(parts[1]), hostPath: isHostPath(parts[0])}
	if len(parts) == 3 {
		m.source += ":" + parts[2]
	}
	if quadlet && strings.HasSuffix(parts[0], ".volume") {
		m.volumeUnit = parts[0]
	}
	return m
}

// parseMount parses a Mount= (or --mount, quadlet false) value
// (type=…,source=…,destination=…). Its source is the spec without the
// destination; a tmpfs, ramfs or devpts mount has none. Without type=,
// Podman mounts a volume.
func parseMount(spec string, quadlet bool) mount {
	var m mount
	var kind, source string
	var rest []string
	for field := range strings.SplitSeq(spec, ",") {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "destination", "dst", "target":
			m.target = path.Clean(value)
			continue
		case "type":
			kind = value
		case "source", "src":
			source = value
		}
		rest = append(rest, field)
	}
	switch kind {
	case "tmpfs", "ramfs", "devpts":
		return m
	case "bind", "volume", "":
		m.hostPath = isHostPath(source)
		if quadlet && strings.HasSuffix(source, ".volume") {
			m.volumeUnit = source
		}
	}
	m.source = strings.Join(rest, ",")
	return m
}

// parseTmpfs parses a Tmpfs= (or --tmpfs) value: target[:options].
func parseTmpfs(tmpfs string) mount {
	dir, _, _ := strings.Cut(tmpfs, ":")
	return mount{target: path.Clean(dir)}
}

// appendSecret appends the mount of a Secret= (or --secret) value; an env
// secret mounts nothing. Its source is the spec without the target.
func appendSecret(mounts []mount, spec string) []mount {
	name, target, mounted := resolver.ParseSecretMount(spec)
	if !mounted {
		return mounts
	}
	fields := slices.DeleteFunc(strings.Split(spec, ","), func(f string) bool { return strings.HasPrefix(f, "target=") })
	return append(mounts, mount{source: "secret " + strings.Join(fields, ","), target: target, secret: name})
}

// isHostPath reports whether a bind or volume source is a Host path:
// absolute, relative to the unit's directory (Quadlet makes it absolute) or
// specifier-led (systemd expands %h to an absolute path); anything else is a
// volume name.
func isHostPath(source string) bool {
	return source != "" && strings.ContainsAny(source[:1], "/.%")
}

// mountsHolding returns the mounts at the deepest target that holds p (the
// container sees the innermost mount; more than one may share the target).
func mountsHolding(mounts []mount, p string) []mount {
	p = path.Clean(p)
	var held []mount
	for _, m := range mounts {
		switch {
		case p != m.target && !strings.HasPrefix(p, strings.TrimSuffix(m.target, "/")+"/"):
		case len(held) == 0 || len(m.target) > len(held[0].target):
			held = []mount{m}
		case m.target == held[0].target:
			held = append(held, m)
		}
	}
	return held
}

// placedTarget reports whether the guard can tell which paths a mount at
// target holds: an absolute target without systemd specifiers. systemd
// expands a specifier (%h, %t, %E) to a path the guard does not know, and
// Podman refuses a relative target; mountsHolding never matches either.
func placedTarget(target string) bool {
	return path.IsAbs(target) && !strings.Contains(target, "%")
}

// unplacedMounts returns the mounts whose target the guard cannot place
// (placedTarget): any of them may hold any path.
func unplacedMounts(mounts []mount) []mount {
	var unplaced []mount
	for _, m := range mounts {
		if !placedTarget(m.target) {
			unplaced = append(unplaced, m)
		}
	}
	return unplaced
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

// flagSpec names podman run flags to look for: their long names, the
// long names of those that take no value (bools; --flag=false carries one),
// and the one-letter flag short, whose long name is shortName.
type flagSpec struct {
	longs     []string
	bools     []string
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

// podmanPathFlags are the podman run flags other than mounts that change
// what the container shows at a path (a --security-opt mask=, a read-only
// root, image volumes, a rootfs) or who reads it (the user, groups and user
// namespace): pathOptions.
var podmanPathFlags = flagSpec{
	longs: []string{
		"security-opt", "image-volume", "user", "userns", "uidmap", "gidmap",
		"subuidname", "subgidname", "group-add",
	},
	bools: []string{"read-only", "read-only-tmpfs", "rootfs", "privileged"},
	short: 'u', shortName: "user",
}

// pathOptions lists the podmanPathFlags unit's PodmanArgs= pass, each as
// "--flag value": the guard cannot tell what they show at a path, so a
// commit that changes them changes what a credential may read as.
func pathOptions(unit *parser.UnitFile) []string {
	var options []string
	podmanArgsValues(unit, podmanPathFlags, func(flag, value string) {
		options = append(options, strings.TrimSpace("--"+flag+" "+value))
	})
	return options
}

// podmanValueShorthands are podman run's one-letter flags that take a value.
const podmanValueShorthands = "acehlmpuvw"

// podmanFlag reports which of spec's podman run flags arg starts, by its
// long name ("" for none), and the value it carries: --flag=value, -vvalue
// or -v=value, also after one-letter boolean flags (-dv). hasValue is false
// when the value is the next argument.
func podmanFlag(arg string, spec flagSpec) (flag, value string, hasValue bool) {
	if long, ok := strings.CutPrefix(arg, "--"); ok {
		name, value, hasValue := strings.Cut(long, "=")
		switch {
		case slices.Contains(spec.bools, name):
			return name, value, true
		case !slices.Contains(spec.longs, name):
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
