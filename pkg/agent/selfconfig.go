package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

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
// The config is the secret the Agent's quadlet mounts at the Agent config
// path (resolver.AgentConfigSecret). While the running Agent reads a
// Fleet-managed config (the secret its unit on disk, read with readFile,
// mounts there is a Managed File), a changeset after which the Agent's next
// start would find no config is refused (checkLostSelfConfig). A Host whose
// Agent config was never Fleet-managed has nothing to check. Otherwise the
// check runs when the changeset writes the mounted secret or the Agent's
// unit: a unit change can repoint the mount at another managed secret that
// is itself unchanged, and the Agent's next start reads that one. The
// credential files are looked up through the new unit's mounts
// (selfConfigLocator).
func CheckSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	quadlet := agentQuadlet(resolved.Files, unit)
	if quadlet == nil {
		return nil
	}
	writesUnit := changeset.Writes(quadlet.DestPath)
	running := quadlet.ParsedUnit // the unit on disk, unless the changeset writes it
	if writesUnit {
		var err error
		if running, err = runningUnit(unit, changeset, readFile); err != nil {
			return err
		}
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
	locate := selfConfigLocator(quadlet.ParsedUnit, running, changeset)
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
// Fleet does not resolve (a renamed, dropped or misspelled config secret),
// or mounts nothing there. The running config is Fleet-managed when the
// secret mounted there by running (the unit on disk; nil when there is
// none) or by next is a Managed File. A Host file mounted there is a
// hand-managed config and passes, as does any config that was never
// Fleet-managed.
func checkLostSelfConfig(next, running *parser.UnitFile, unit string, changeset *reconciler.Changeset) error {
	managed, fleetManaged := managedConfigSecret(running, changeset)
	nextName, mounted := resolver.AgentConfigMountOf(next)
	if mounted && changeset.Managed(resolver.SecretDestPath(nextName)) {
		return fmt.Errorf("refusing to remove the Agent config secret %s: %s still mounts it at %s, so the Agent could not start; "+
			"keep resolving the secret, or change the mount in the same commit", nextName, unit, resolver.AgentConfigTarget)
	}
	if !fleetManaged {
		return nil
	}
	if mounted {
		return fmt.Errorf("refusing the Agent unit %s: it mounts the secret %s at %s, which the Fleet does not resolve, "+
			"while the running Agent reads the Fleet-managed config secret %s there, so the Agent could not start; "+
			"resolve %s in the Agent bundle, or keep mounting %s", unit, nextName, resolver.AgentConfigTarget, managed, nextName, managed)
	}
	if m, _, ok := mountHolding(unitMounts(next), resolver.AgentConfigTarget); ok && m.source.shared() {
		return nil
	}
	return fmt.Errorf("refusing the Agent unit %s: it mounts nothing at %s, while the running Agent reads the Fleet-managed config secret %s there, "+
		"so the Agent could not start; keep the Secret= mount, or mount a Host file there with Volume= to manage the config by hand",
		unit, resolver.AgentConfigTarget, managed)
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
