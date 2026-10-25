package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// mount is one mount of the Agent quadlet's container: what it mounts
// (source) and where (target, inside the container).
type mount struct {
	// source is the Volume=/Mount= source (a host path or a volume name),
	// or "secret <name>" for a Secret=; "" for an anonymous volume or a
	// tmpfs, which no other container shares.
	source string
	target string
}

// unitMounts lists the mounts unit's [Container] group declares: Volume=,
// Mount= of type bind or volume, and Secret= (type=mount, the default).
// Sources are compared as written, so the same specifier (%h) in two
// units matches.
func unitMounts(unit *parser.UnitFile) []mount {
	var mounts []mount
	for _, volume := range unit.LookupAll("Container", "Volume") {
		parts := strings.Split(volume, ":")
		if len(parts) == 1 {
			mounts = append(mounts, mount{target: cleanTarget(parts[0])})
			continue
		}
		mounts = append(mounts, mount{source: cleanSource(parts[0]), target: cleanTarget(parts[1])})
	}
	for _, spec := range unit.LookupAll("Container", "Mount") {
		if m, ok := parseMount(spec); ok {
			mounts = append(mounts, m)
		}
	}
	for _, secret := range unit.LookupAll("Container", "Secret") {
		if m, ok := parseSecretMount(secret); ok {
			mounts = append(mounts, m)
		}
	}
	return mounts
}

// parseMount parses a Mount= value (type=bind,source=…,destination=…).
// Mounts that place no file from elsewhere (tmpfs, devpts, …) are not
// listed.
func parseMount(spec string) (mount, bool) {
	var kind string
	var m mount
	for field := range strings.SplitSeq(spec, ",") {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "type":
			kind = value
		case "source", "src":
			m.source = cleanSource(value)
		case "destination", "dst", "target":
			m.target = cleanTarget(value)
		}
	}
	return m, (kind == "bind" || kind == "volume") && m.target != ""
}

// parseSecretMount parses a Secret= value (name[,type=…][,target=…]): a
// file secret is mounted at its target, by default /run/secrets/<name>, and
// a relative target is under /run/secrets.
func parseSecretMount(spec string) (mount, bool) {
	fields := strings.Split(spec, ",")
	name := fields[0]
	target := name
	for _, field := range fields[1:] {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "type":
			if value != "mount" {
				return mount{}, false
			}
		case "target":
			target = value
		}
	}
	if !path.IsAbs(target) {
		target = path.Join("/run/secrets", target)
	}
	return mount{source: "secret " + name, target: path.Clean(target)}, true
}

// cleanSource normalizes a host path source; a volume name is kept.
func cleanSource(source string) string {
	if strings.HasPrefix(source, "/") {
		return path.Clean(source)
	}
	return source
}

func cleanTarget(target string) string { return path.Clean(target) }

// mountHolding returns the mount of mounts whose target holds p (the
// deepest, as the container sees the innermost mount), and p relative to
// that target.
func mountHolding(mounts []mount, p string) (m mount, rel string, ok bool) {
	p = path.Clean(p)
	for _, candidate := range mounts {
		r, under := relativeTo(candidate.target, p)
		if under && (!ok || len(candidate.target) > len(m.target)) {
			m, rel, ok = candidate, r, true
		}
	}
	return m, rel, ok
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
// container with the mounts of current. A credential path under a mount of
// next is read through the mount of current with the same source (at its
// target); one under no mount of next is inside the image and read where it
// is spelled, unless current mounts it: next drops that mount. A file
// credential the running container cannot see is refused; a directory
// credential (protonpass.session_dir) need not exist, so it is read where it
// is spelled instead.
func credentialLocator(next, current []mount) agentcfg.Locate {
	return func(cred agentcfg.Credential) (string, error) {
		m, rel, ok := mountHolding(next, cred.Path)
		if !ok {
			if held, _, ok := mountHolding(current, cred.Path); ok && !cred.Dir {
				return "", fmt.Errorf("the running Agent's container mounts %s from %s, which the new Agent unit no longer mounts; %s",
					held.target, describeSource(held.source), errMountChange)
			}
			return cred.Path, nil
		}
		if m.source != "" {
			for _, c := range current {
				if c.source == m.source {
					return path.Join(c.target, rel), nil
				}
			}
		}
		if cred.Dir {
			return cred.Path, nil
		}
		return "", fmt.Errorf("the new Agent unit mounts %s from %s, which the running Agent's container does not mount; %s",
			m.target, describeSource(m.source), errMountChange)
	}
}

func describeSource(source string) string {
	if source == "" {
		return "an anonymous volume"
	}
	return source
}

// selfConfigLocator returns how to read the credentials of the Agent config
// the Agent's next start reads. When the changeset writes the Agent's unit
// (its Quadlet file, newUnit), that start has newUnit's mounts while the
// running Agent has those of the unit on disk (read with readFile), so
// credentials are located through both (credentialLocator). Otherwise, or
// when no unit of the Agent is on disk (the Agent runs from a unit the Fleet
// does not manage), credentials are read where they are spelled.
func selfConfigLocator(files []resolver.ResolvedFile, unit string, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (agentcfg.Locate, error) {
	newUnit := writtenUnit(files, unit, changeset)
	if newUnit == nil {
		return nil, nil //nolint:nilnil // a nil Locate reads credentials where they are spelled
	}
	current, found, err := currentUnit(unit, changeset, readFile)
	if err != nil || !found {
		return nil, err
	}
	return credentialLocator(unitMounts(newUnit), unitMounts(current)), nil
}

// writtenUnit returns the parsed Quadlet file of unit when the changeset
// writes it.
func writtenUnit(files []resolver.ResolvedFile, unit string, changeset *reconciler.Changeset) *parser.UnitFile {
	for _, file := range files {
		if file.ServiceName == unit && file.Category == config.CategoryContainer && changeset.Writes(file.DestPath) {
			return file.ParsedUnit
		}
	}
	return nil
}

// currentUnit reads and parses the Agent's container Quadlet file on disk: the
// file the changeset updates or deletes for unit (a rename creates a new
// file and deletes the old one). found is false when there is none.
func currentUnit(unit string, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (parsed *parser.UnitFile, found bool, err error) {
	for _, change := range changeset.Changes {
		if change.ServiceName != unit || change.Category != config.CategoryContainer || change.Action == reconciler.ActionCreate {
			continue
		}
		data, err := readFile(change.DestPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading the running Agent's unit %s: %w", change.DestPath, err)
		}
		parsed := parser.NewUnitFile()
		parsed.Filename = path.Base(change.DestPath)
		if err := parsed.Parse(string(data)); err != nil {
			return nil, false, fmt.Errorf("parsing the running Agent's unit %s: %w", change.DestPath, err)
		}
		return parsed, true, nil
	}
	return nil, false, nil
}
