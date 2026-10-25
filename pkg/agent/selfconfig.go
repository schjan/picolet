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

// CheckSelfConfig is the self-restart guard: it refuses a changeset that
// would leave the Agent's own Fleet-rendered config one the Agent could not
// start with: one its strict loader rejects or one whose startup credential
// checks fail (agentcfg.CheckRenderedConfig), or none at all. A changed
// config does not restart the Agent, so applying it would only lock the
// Agent out at its next restart, when it can no longer fetch a fix. The
// Agent runs it before every apply, and so does `picolet apply --config`.
//
// Every changeset that removes a Fleet-managed secret or .volume unit the
// Agent's next unit mounts is refused (checkLostSources). The config is the
// secret the Agent's quadlet mounts at the Agent config path
// (resolver.AgentConfigSecret). While the running Agent reads a
// Fleet-managed config (the secret its unit on disk, read with readFile,
// mounts there is a Managed File), a changeset after which the Agent's next
// start would find no config is refused (checkLostSelfConfig). A Host whose
// Agent config was never Fleet-managed has nothing to check. Otherwise the
// config check runs when the changeset writes the mounted secret or the
// Agent's unit: a unit change can repoint the mount at another managed
// secret that is itself unchanged, and the Agent's next start reads that
// one. The credential files are looked up through the new unit's mounts
// (selfConfigLocator); getwd is the running Agent's working directory
// (os.Getwd), which a relative path may resolve against (nextWorkingDir).
//
// A changeset whose resolve renders no Agent quadlet (the Agent bundle
// unassigned, or its container quadlet dropped) decommissions the Fleet's
// Agent unit: the guard does not check it, and the apply deletes the unit
// and the config secret it mounts.
func CheckSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error), getwd func() (string, error)) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	quadlet := agentQuadlet(resolved.Files, unit)
	if quadlet == nil {
		return nil
	}
	if err := checkLostSources(quadlet.ParsedUnit, unit, changeset); err != nil {
		return err
	}
	writesUnit := changeset.Writes(quadlet.DestPath)
	running, err := runningAgentUnit(quadlet, writesUnit, changeset, readFile)
	if err != nil {
		return err
	}
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return checkLostSelfConfig(quadlet.ParsedUnit, running, unit, changeset)
	}
	if err != nil {
		return fmt.Errorf("finding the Agent config mounted by %s: %w", unit, err)
	}
	if !changeset.Writes(file.DestPath) && !writesUnit {
		return nil
	}
	locate := selfConfigLocator(quadlet.ParsedUnit, running, writesUnit, changeset, getwd)
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content), locate); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// agentQuadlet returns the resolved container quadlet generating unit.
func agentQuadlet(files []resolver.ResolvedFile, unit string) *resolver.ResolvedFile {
	for i := range files {
		if files[i].ServiceName == unit && files[i].Category == config.CategoryContainer && files[i].ParsedUnit != nil {
			return &files[i]
		}
	}
	return nil
}

// checkLostSelfConfig refuses a changeset after which the Agent's next start,
// with the unit next, would find no config at the Agent config path while
// the running Agent reads a Fleet-managed one: next mounts a secret the
// Fleet does not resolve (a misspelled or never-resolved config secret;
// one the changeset removes is refused by checkLostSources), mounts
// nothing there, or mounts something other than a Host path there (a
// volume, tmpfs, image or glob mount, which holds no config of its own).
// The running config is Fleet-managed when the secret mounted there by
// running (the unit on disk; nil when there is none) is a Managed File. A
// Host file or directory bind-mounted there is a hand-managed config and
// passes, as does any config that was never Fleet-managed.
func checkLostSelfConfig(next, running *parser.UnitFile, unit string, changeset *reconciler.Changeset) error {
	managed, fleetManaged := managedConfigSecret(running, changeset)
	if !fleetManaged {
		return nil
	}
	if nextName, mounted := resolver.AgentConfigMountOf(next); mounted {
		return fmt.Errorf("refusing the Agent unit %s: it mounts the secret %s at %s, which the Fleet does not resolve, "+
			"while the running Agent reads the Fleet-managed config secret %s there, so the Agent could not start; "+
			"resolve %s in the Agent bundle, or keep mounting %s", unit, nextName, resolver.AgentConfigTarget, managed, nextName, managed)
	}
	const fix = "keep the Secret= mount, or bind-mount a Host file there with Volume= to manage the config by hand"
	mounts, _ := unitMounts(next)
	m, _, ok := mountHolding(mounts, resolver.AgentConfigTarget)
	switch {
	case ok && isHostPath(m.source):
		return nil
	case ok && m.source.shared():
		return fmt.Errorf("refusing the Agent unit %s: it mounts %s at %s, not a Host path, while the running Agent reads the Fleet-managed config secret %s there, "+
			"so the Agent could not start; %s", unit, m.source, m.target, managed, fix)
	}
	return fmt.Errorf("refusing the Agent unit %s: it mounts nothing at %s, while the running Agent reads the Fleet-managed config secret %s there, "+
		"so the Agent could not start; %s", unit, resolver.AgentConfigTarget, managed, fix)
}

// isHostPath reports whether source is a bind mount of a Host path: an
// absolute one, one relative to the unit's directory (Quadlet makes it
// absolute) or one starting with a systemd specifier such as %h (systemd
// expands it to an absolute path); anything else is a volume name.
func isHostPath(source mountSource) bool {
	return source.kind == sourceVolume && strings.ContainsAny(source.name[:1], "/.%")
}

// managedConfigSecret returns the secret unit mounts at the Agent config path
// when it is a Managed File before the changeset.
func managedConfigSecret(unit *parser.UnitFile, changeset *reconciler.Changeset) (string, bool) {
	if unit == nil {
		return "", false
	}
	name, ok := resolver.AgentConfigMountOf(unit)
	if !ok || !changeset.Managed(resolver.SecretDestPath(name)) {
		return "", false
	}
	return name, true
}

// runningAgentUnit returns the Agent's unit on disk: the resolved quadlet,
// unless the changeset writes it (writesUnit), and then the file it replaces
// (runningUnit).
func runningAgentUnit(quadlet *resolver.ResolvedFile, writesUnit bool, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (*parser.UnitFile, error) {
	if !writesUnit {
		return quadlet.ParsedUnit, nil
	}
	return runningUnit(quadlet.ServiceName, changeset, readFile)
}

// runningUnit reads and parses the Agent's container Quadlet file on disk:
// the file the changeset updates or deletes for unit (a rename creates a
// new file and deletes the old one). It returns nil when there is none.
func runningUnit(unit string, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (*parser.UnitFile, error) {
	for _, change := range changeset.Changes {
		if change.ServiceName != unit || change.Category != config.CategoryContainer || change.Action == reconciler.ActionCreate {
			continue
		}
		data, readErr := readFile(change.DestPath)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading the running Agent's unit %s: %w", change.DestPath, readErr)
		}
		running := parser.NewUnitFile()
		running.Filename = path.Base(change.DestPath)
		if err := running.Parse(string(data)); err != nil {
			return nil, fmt.Errorf("parsing the running Agent's unit %s: %w", change.DestPath, err)
		}
		return running, nil
	}
	return nil, nil //nolint:nilnil // no unit of the Agent on disk
}
