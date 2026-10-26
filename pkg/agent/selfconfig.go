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
// apply, and so does `picolet apply --config`. Its policy:
//
//   - Every changeset that removes a Fleet-managed secret or .volume unit the
//     Agent's next unit uses is refused (checkLostSources). This also refuses
//     losing a Fleet-managed config: with the mount lines frozen, the next
//     start mounts the same config secret, which the Fleet then no longer
//     resolves and the changeset removes.
//   - A changeset that writes the Agent's unit may not change its frozenKeys
//     lines from the unit on disk (checkFrozenLines): the Agent's mounts,
//     identity and access change on the Host, then through the Fleet.
//   - A changeset that writes the config (the secret the next unit mounts at
//     the Agent config path, resolver.AgentConfigSecret) or the Agent's unit
//     must leave a config that passes agentcfg.CheckRenderedConfig, with
//     every credential path absolute (absolutePath). With a unit on disk,
//     whose mounts the next start keeps, each credential is read where it is
//     spelled, unless the running container may show it otherwise than a
//     fresh start (sameView).
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
	running, err := runningAgentUnit(quadlet, writesUnit, resolved.Host.Rootful(), changeset, readFile)
	if err != nil {
		return err
	}
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return nil // a config the Fleet never managed, or one checkLostSources keeps
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
// the unit next: without a unit on disk (running nil), whose mounts are
// unknown, its credentials are read where they are spelled, as at the
// Agent's startup; with one, whose frozen mounts the next start keeps, in
// the running container too, where a fresh start would show them alike
// (sameView).
func checkNextConfig(next, running *parser.UnitFile, file *resolver.ResolvedFile, unit string, changeset *reconciler.Changeset) error {
	locate := absolutePath
	if running != nil {
		locate = sameView(next, changeset)
	}
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content), locate); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// runningAgentUnit returns the Agent's unit on disk: the resolved quadlet,
// unless the changeset writes it (writesUnit), and then the file it replaces
// (runningUnit; nil when there is none), whose frozen lines the quadlet must
// keep (checkFrozenLines).
func runningAgentUnit(quadlet *resolver.ResolvedFile, writesUnit, rootful bool, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (*parser.UnitFile, error) {
	if !writesUnit {
		return quadlet.ParsedUnit, nil
	}
	running, err := runningUnit(quadlet.ServiceName, changeset, readFile)
	if err != nil || running == nil {
		return running, err
	}
	return running, checkFrozenLines(running, quadlet.ParsedUnit, quadlet.ServiceName, quadlet.DestPath, rootful)
}

// frozenKeys are the Agent unit's [Container] keys the Fleet may not change
// (checkFrozenLines): every key that decides what paths the container shows
// (mounts, masks, the root file system, the working directory), who reads
// them (user, groups, user namespace and ID maps, capabilities, AppArmor and
// SELinux labels), or what podman adds to either (PodmanArgs=, GlobalArgs=,
// ContainersConfModule=). Image= is not frozen: the Agent updates itself.
var frozenKeys = []string{
	"Volume", "Mount", "Tmpfs", "Secret", "Mask", "Unmask", "VolatileTmp",
	"ReadOnly", "ReadOnlyTmpfs", "Rootfs", "WorkingDir",
	"User", "Group", "GroupAdd", "UserNS", "UIDMap", "GIDMap", "SubUIDMap", "SubGIDMap",
	"RemapUsers", "RemapUid", "RemapGid", "RemapUidSize",
	"AddCapability", "DropCapability", "AppArmor",
	"SecurityLabelDisable", "SecurityLabelFileType", "SecurityLabelLevel", "SecurityLabelNested", "SecurityLabelType",
	"PodmanArgs", "GlobalArgs", "ContainersConfModule",
}

// checkFrozenLines refuses a next Agent unit whose frozenKeys lines differ
// from those of running, the unit on disk the running container was started
// from: each key's values, trimmed, in order, as written. The guard checks
// the next start in the running container, so it could not check one with
// other mounts or another identity; the Host's operator changes them by hand
// (at quadletPath) and restarts the Agent first (rootful: the Host's system
// manager), and the Fleet then commits the same lines.
func checkFrozenLines(running, next *parser.UnitFile, unit, quadletPath string, rootful bool) error {
	var changed []string
	for _, key := range frozenKeys {
		if !slices.Equal(trimmedValues(running, key), trimmedValues(next, key)) {
			changed = append(changed, key+"=")
		}
	}
	if len(changed) == 0 {
		return nil
	}
	restart := "systemctl --user restart " + unit + ", as the Host's user"
	if rootful {
		restart = "systemctl restart " + unit
	}
	return fmt.Errorf("refusing the Agent unit %s: this commit changes its %s lines, and the Agent unit's mounts can't change through the Fleet: "+
		"edit %s on the Host to the new lines, restart the Agent (%s), then commit the same lines",
		unit, strings.Join(changed, ", "), quadletPath, restart)
}

// trimmedValues returns unit's [Container] values of key, each trimmed.
func trimmedValues(unit *parser.UnitFile, key string) []string {
	values := unit.LookupAll("Container", key)
	for i, v := range values {
		values[i] = strings.TrimSpace(v)
	}
	return values
}

// absolutePath is the agentcfg.Locate of a Fleet-rendered Agent config: it
// refuses a relative credential path, whose file the guard cannot tell the
// next start finds, and reads the rest where they are spelled.
func absolutePath(cred agentcfg.Credential) (string, error) {
	if !path.IsAbs(cred.Path) {
		return "", errors.New("the path is relative; make it absolute")
	}
	return cred.Path, nil
}

// sameView returns the agentcfg.Locate of a changeset checked in the running
// container, which the next start, with the same mounts (checkFrozenLines),
// replaces: a credential path must be absolute, and a file credential is
// refused where a fresh start of the unit next may show it otherwise than the
// running container: when next passes --volumes-from (the guard cannot read
// what it mounts), or under a tmpfs or anonymous volume (Quadlet runs the
// container with --rm, so it starts empty), or under a mount whose
// Fleet-managed secret or .volume unit the changeset writes (the running
// container still shows the old content). The mounts that may hold a path
// are those at the deepest target holding it and every mount whose target
// the guard cannot place (relative or specifier-led). A directory credential
// (protonpass.session_dir) need not exist, so it need only be absolute.
func sameView(next *parser.UnitFile, changeset *reconciler.Changeset) agentcfg.Locate {
	mounts, volumesFrom := unitMounts(next)
	return func(cred agentcfg.Credential) (string, error) {
		if _, err := absolutePath(cred); err != nil || cred.Dir {
			return cred.Path, err
		}
		if len(volumesFrom) > 0 {
			return "", fmt.Errorf("the Agent unit passes %s in PodmanArgs=, which mounts what the guard cannot read; "+
				"keep credentials on a Host path or in a secret", strings.Join(volumesFrom, ", "))
		}
		for _, m := range mayHold(mounts, cred.Path) {
			switch {
			case m.empty:
				return "", fmt.Errorf("the Agent unit mounts a tmpfs or anonymous volume at %s, which may hold it and starts empty in a new container; "+
					"keep credentials on a Host path or in a secret", m.target)
			case m.secret != "" && changeset.Writes(resolver.SecretDestPath(m.secret)):
				return "", changedSource("the secret " + m.secret)
			case m.volumeUnit != "" && writesVolumeUnit(changeset, m.volumeUnit):
				return "", changedSource("the volume unit " + m.volumeUnit)
			}
		}
		return cred.Path, nil
	}
}

// changedSource is sameView's refusal of a credential mounted from what, a
// source the changeset writes.
func changedSource(what string) error {
	return fmt.Errorf("the Agent unit mounts it from %s, which this commit changes; change %s in its own commit, then the config", what, what)
}

// writesVolumeUnit reports whether the changeset changes the .volume unit
// named name.
func writesVolumeUnit(changeset *reconciler.Changeset, name string) bool {
	return slices.ContainsFunc(volumeUnitChanges(changeset, name), func(c reconciler.Change) bool {
		return c.Action != reconciler.ActionNoop
	})
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
