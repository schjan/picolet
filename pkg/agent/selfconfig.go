package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// CheckSelfConfig is the self-restart guard: it refuses a changeset that
// would leave the Agent a config it could not start with at its next
// restart, when it can no longer fetch a fix. The Agent runs it before every
// apply, and so does `picolet apply --config`. Its policy is conservative:
//
//   - Every changeset that removes a Fleet-managed secret or .volume unit the
//     Agent's next unit uses is refused (checkLostSources).
//   - While the running Agent reads a Fleet-managed config (the secret its
//     unit on disk, read with readFile, mounts at the Agent config path is a
//     Managed File), a changeset after which the next start would find no
//     config is refused (checkLostSelfConfig).
//   - A changeset that writes the config (the secret the next unit mounts at
//     the Agent config path, resolver.AgentConfigSecret) or the Agent's unit
//     must leave a config that passes agentcfg.CheckRenderedConfig, with
//     every credential path absolute (absolutePath).
//   - A changeset that changes the Agent unit's mount layout
//     (sameMountLayout) may not also change the config or a Fleet-managed
//     source the unit mounts (checkRelayoutAlone), and every credential must
//     lie under a mount the new unit keeps as it was, or under none
//     (keptMounts): the running container then sees each credential as the
//     next start will.
//
// A changeset whose resolve renders no Agent quadlet (the Agent bundle
// unassigned, or its container quadlet dropped) decommissions the Fleet's
// Agent unit: the guard does not check it.
func CheckSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	quadlet := agentQuadlet(resolved.Files, unit)
	if quadlet == nil {
		return nil
	}
	next := quadlet.ParsedUnit
	if err := checkLostSources(next, unit, changeset); err != nil {
		return err
	}
	writesUnit := changeset.Writes(quadlet.DestPath)
	running, err := runningAgentUnit(quadlet, writesUnit, changeset, readFile)
	if err != nil {
		return err
	}
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return checkLostSelfConfig(next, running, unit, changeset)
	}
	if err != nil {
		return fmt.Errorf("finding the Agent config mounted by %s: %w", unit, err)
	}
	if !writesUnit && !changeset.Writes(file.DestPath) {
		return nil
	}
	return checkNextConfig(next, running, file, unit, changeset)
}

// checkNextConfig checks file, the config the Agent's next start reads with
// the unit next, against the running Agent's unit running: with an
// unchanged mount layout (or no unit on disk, whose mounts are unknown) its
// credentials are read where they are spelled, as at the Agent's startup;
// a changed one must change alone (checkRelayoutAlone) and keep every
// credential's mounts (keptMounts).
func checkNextConfig(next, running *parser.UnitFile, file *resolver.ResolvedFile, unit string, changeset *reconciler.Changeset) error {
	locate := absolutePath
	if running != nil && !sameMountLayout(running, next) {
		if err := checkRelayoutAlone(next, file.DestPath, unit, changeset); err != nil {
			return err
		}
		locate = keptMounts(running, next)
	}
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content), locate); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// errOwnCommit ends every refusal of a mount change the guard cannot check.
const errOwnCommit = "change the Agent's mounts in their own commit, then the config"

// absolutePath is the agentcfg.Locate of a Fleet-rendered Agent config: it
// refuses a relative credential path, whose file the guard cannot tell the
// next start finds, and reads the rest where they are spelled.
func absolutePath(cred agentcfg.Credential) (string, error) {
	if !path.IsAbs(cred.Path) {
		return "", errors.New("the path is relative; make it absolute")
	}
	return cred.Path, nil
}

// keptMounts returns the agentcfg.Locate of a changeset that changes the
// Agent unit's mounts from running's to next's: a file credential must lie
// under the same mounts (target and source as written, no tmpfs) in both
// units, or under none (in the image), so the running container sees it as
// the next start will. A mount the guard cannot read (--volumes-from)
// refuses every file credential. A directory credential
// (protonpass.session_dir) need not exist, so it need only be absolute.
func keptMounts(running, next *parser.UnitFile) agentcfg.Locate {
	nextMounts, nextOpaque := unitMounts(next)
	runningMounts, runningOpaque := unitMounts(running)
	opaque := slices.Concat(nextOpaque, runningOpaque)
	return func(cred agentcfg.Credential) (string, error) {
		if _, err := absolutePath(cred); err != nil || cred.Dir {
			return cred.Path, err
		}
		if len(opaque) > 0 {
			return "", fmt.Errorf("the Agent unit passes %s in PodmanArgs=, which mounts what the guard cannot read; %s",
				strings.Join(opaque, ", "), errOwnCommit)
		}
		held := mountsHolding(nextMounts, cred.Path)
		if !slices.Equal(held, mountsHolding(runningMounts, cred.Path)) ||
			slices.ContainsFunc(held, func(m mount) bool { return m.source == "" }) {
			return "", fmt.Errorf("this commit changes the Agent unit's mounts, and the path is not under a mount the new unit keeps as it was; %s", errOwnCommit)
		}
		return cred.Path, nil
	}
}

// checkRelayoutAlone refuses a changeset that changes the Agent unit's mount
// layout together with something the Agent reads at start: its config
// secret (configDest), or a Fleet-managed secret or .volume unit the new
// unit next uses. The running container shows their old content, so the
// guard could not check the next start.
func checkRelayoutAlone(next *parser.UnitFile, configDest, unit string, changeset *reconciler.Changeset) error {
	refuse := func(what string) error {
		return fmt.Errorf("refusing the Agent unit %s: this commit changes its mounts and also %s, which the Agent reads at start; %s",
			unit, what, errOwnCommit)
	}
	if changeset.Writes(configDest) {
		return refuse("its config " + configDest)
	}
	for _, secret := range unitSecrets(next) {
		if changeset.Writes(resolver.SecretDestPath(secret.name)) {
			return refuse("the secret " + secret.name)
		}
	}
	mounts, _ := unitMounts(next)
	for _, m := range mounts {
		if m.volumeUnit != "" && slices.ContainsFunc(volumeUnitChanges(changeset, m.volumeUnit), func(c reconciler.Change) bool {
			return c.Action != reconciler.ActionNoop
		}) {
			return refuse("the volume unit " + m.volumeUnit)
		}
	}
	return nil
}

// checkLostSources refuses a changeset that removes a source the Agent's next
// start, with the unit next, needs: a Fleet-managed Podman secret a Secret=
// or a PodmanArgs= --secret names (of either type), or a Fleet-managed
// .volume unit a Volume= or Mount= names. Podman could not create the
// Agent's container without the secret, nor Quadlet generate its service
// without the .volume unit, so the Agent would not start again. It runs on
// every changeset, whether or not it writes the Agent's config or unit.
func checkLostSources(next *parser.UnitFile, unit string, changeset *reconciler.Changeset) error {
	for _, secret := range unitSecrets(next) {
		if changeset.Deletes(resolver.SecretDestPath(secret.name)) {
			return fmt.Errorf("refusing to remove the secret %s: the Agent unit %s still mounts it (%s), so the Agent could not start; "+
				"keep resolving the secret, or stop mounting it", secret.name, unit, secret.decl)
		}
	}
	mounts, _ := unitMounts(next)
	for _, m := range mounts {
		if m.volumeUnit != "" && removesVolumeUnit(changeset, m.volumeUnit) {
			return fmt.Errorf("refusing to remove the volume unit %s: the Agent unit %s still mounts it at %s, so the Agent could not start; "+
				"keep the volume unit, or stop mounting it", m.volumeUnit, unit, m.target)
		}
	}
	return nil
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
	held := mountsHolding(mounts, resolver.AgentConfigTarget)
	switch {
	case len(held) > 0 && held[0].hostPath:
		return nil
	case len(held) > 0 && held[0].source != "":
		return fmt.Errorf("refusing the Agent unit %s: it mounts %s at %s, not a Host path, while the running Agent reads the Fleet-managed config secret %s there, "+
			"so the Agent could not start; %s", unit, held[0].source, held[0].target, managed, fix)
	}
	return fmt.Errorf("refusing the Agent unit %s: it mounts nothing at %s, while the running Agent reads the Fleet-managed config secret %s there, "+
		"so the Agent could not start; %s", unit, resolver.AgentConfigTarget, managed, fix)
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

// selfConfigRestart returns the Applier option that restarts the Agent's
// unit after an apply that writes the Agent's Fleet-rendered config (the
// secret its next unit mounts at the Agent config path): the running Agent
// never reloads its config, and Podman copies the secret into the container
// only when it creates it. CheckSelfConfig has already checked that config,
// so the restart cannot lock the Agent out. It returns none otherwise.
func selfConfigRestart(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset) []applier.Option {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if err != nil || !changeset.Writes(file.DestPath) {
		return nil
	}
	return []applier.Option{applier.WithSelfConfig(unit, reconciler.SecretNameFromPath(file.DestPath))}
}
