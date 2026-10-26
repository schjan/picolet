package agent

import (
	"path"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/resolver"
)

// mount is one mount of the Agent unit's container, as the self-restart
// guard needs it: its target inside the container, whether a fresh start
// finds it empty (a tmpfs or anonymous volume) or mounts an image, and the
// Fleet-managed source it shows (a Podman secret, or a .volume unit).
type mount struct {
	target string
	empty  bool
	image  bool
	// volumeUnit names the .volume unit a Volume= or Mount= mounts.
	volumeUnit string
	// secret names the Podman secret a Secret= or --secret mounts.
	secret string
}

// unitMounts lists the mounts unit's [Container] group declares, read as
// Quadlet reads them: Volume=, Mount=, Tmpfs=, Secret= (type=mount) and
// VolatileTmp='s tmpfs at /tmp, then the mounts its PodmanArgs= pass to
// podman run (-v/--volume, --mount, --tmpfs, --secret; a .volume source
// there is a volume name, not a unit). It returns apart the --volumes-from
// options, each as "--volumes-from value": what they mount the guard cannot
// read. A flag is read wherever it appears, even as another flag's value, so
// a mount is never missed.
func unitMounts(unit *parser.UnitFile) (mounts []mount, volumesFrom []string) {
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
	if unit.LookupBooleanWithDefault("Container", "VolatileTmp", false) {
		mounts = append(mounts, mount{target: "/tmp", empty: true})
	}
	podmanMounts, volumesFrom := podmanArgsMounts(unit)
	return append(mounts, podmanMounts...), volumesFrom
}

// podmanArgsMounts lists the mounts unit's PodmanArgs= pass to podman run,
// and apart its --volumes-from options (unitMounts).
func podmanArgsMounts(unit *parser.UnitFile) (mounts []mount, volumesFrom []string) {
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
			volumesFrom = append(volumesFrom, "--"+flag+" "+value)
		}
	})
	return mounts, volumesFrom
}

// parseVolume parses a Volume= (or -v, quadlet false) value:
// source:target[:options], or a target alone. A target alone, or (in
// Quadlet, which then passes the target alone) an empty source, is an
// anonymous volume.
func parseVolume(volume string, quadlet bool) mount {
	parts := strings.SplitN(volume, ":", 3)
	if len(parts) == 1 {
		return mount{target: path.Clean(parts[0]), empty: true}
	}
	m := mount{target: path.Clean(parts[1]), empty: parts[0] == ""}
	if quadlet && strings.HasSuffix(parts[0], ".volume") {
		m.volumeUnit = parts[0]
	}
	return m
}

// parseMount parses a Mount= (or --mount, quadlet false) value
// (type=…,source=…,destination=…). A tmpfs, ramfs or devpts mount starts
// empty, as does a volume without a source (an anonymous volume, whatever
// its options). Without type=, Podman mounts a volume.
func parseMount(spec string, quadlet bool) mount {
	var kind, source string
	var m mount
	for field := range strings.SplitSeq(spec, ",") {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "destination", "dst", "target":
			m.target = path.Clean(value)
		case "type":
			kind = value
		case "source", "src":
			source = value
		}
	}
	switch kind {
	case "tmpfs", "ramfs", "devpts":
		m.empty = true
	case "volume", "":
		m.empty = source == ""
	case "image":
		m.image = true
	}
	if quadlet && strings.HasSuffix(source, ".volume") {
		m.volumeUnit = source
	}
	return m
}

// parseTmpfs parses a Tmpfs= (or --tmpfs) value: target[:options].
func parseTmpfs(tmpfs string) mount {
	dir, _, _ := strings.Cut(tmpfs, ":")
	return mount{target: path.Clean(dir), empty: true}
}

// appendSecret appends the mount of a Secret= (or --secret) value; an env
// secret mounts nothing.
func appendSecret(mounts []mount, spec string) []mount {
	name, target, mounted := resolver.ParseSecretMount(spec)
	if !mounted {
		return mounts
	}
	return append(mounts, mount{target: target, secret: name})
}

// under reports whether p is target or lies below it.
func under(p, target string) bool {
	return p == target || strings.HasPrefix(p, strings.TrimSuffix(target, "/")+"/")
}

// mayHold returns the mounts that may hold p: those at the deepest target
// that holds it (the container sees the innermost mount; more than one may
// share the target), then every mount whose target the guard cannot place
// (relative, or with a systemd specifier such as %h that expands to a path
// it does not know), which may hold any path.
func mayHold(mounts []mount, p string) []mount {
	p = path.Clean(p)
	var held, unplaced []mount
	for _, m := range mounts {
		switch {
		case !placed(m):
			unplaced = append(unplaced, m)
		case !under(p, m.target):
		case len(held) == 0 || len(m.target) > len(held[0].target):
			held = []mount{m}
		case m.target == held[0].target:
			held = append(held, m)
		}
	}
	return append(held, unplaced...)
}

// placed reports whether the guard can place m's target: an absolute path
// without a systemd specifier.
func placed(m mount) bool {
	return path.IsAbs(m.target) && !strings.Contains(m.target, "%")
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
