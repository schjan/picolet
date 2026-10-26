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
//   - The Agent's quadlet must be the only Fleet unit that is the Agent's
//     service (checkOneAgentService): the guard checks no other.
//   - The unit the Agent's next start uses (agentUnits.next) is the Fleet's
//     render when the changeset writes the Agent's quadlet, else the unit on
//     disk, which may hold a Host-side edit the Fleet has not committed yet.
//   - Every changeset that removes a Fleet-managed secret or .volume unit the
//     next unit uses is refused (checkLostSources). With a unit on disk,
//     whose config mount the next unit keeps (checkFrozenLines), this also
//     refuses losing a Fleet-managed config.
//   - A changeset that writes the Agent's unit may not change its frozen
//     lines (frozenKeys, PICOLET_CONFIG, [Service]) from the unit on disk
//     (checkFrozenLines): the Agent's mounts, identity and access change on
//     the Host, then through the Fleet.
//   - A changeset that writes the config (agentConfigFile: the secret the
//     next unit mounts where the Agent reads its config) or the Agent's unit
//     must leave a config that passes agentcfg.CheckRenderedConfig, with
//     every credential path absolute (absolutePath). With a unit on disk,
//     whose mounts the next start keeps, each credential is read where it is
//     spelled, unless a fresh start may show it otherwise than the running
//     container (sameView).
//
// A changeset whose resolve renders no Agent quadlet decommissions the
// Fleet's Agent unit, and the guard does not check it, unless it keeps the
// Agent's unit on disk in another shape (checkAgentServiceKept).
func CheckSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	quadlet := agentQuadlet(resolved.Files, unit)
	if quadlet == nil {
		return checkAgentServiceKept(resolved, unit, changeset)
	}
	if err := checkOneAgentService(resolved.Files, quadlet, unit); err != nil {
		return err
	}
	units, err := agentUnitsOf(quadlet, changeset, readFile)
	if err != nil {
		return err
	}
	if err := checkNextUnit(units, unit, resolved.Host.Rootful(), changeset); err != nil {
		return err
	}
	file := agentConfigFile(resolved.Files, units.next)
	if file == nil || !units.writes && !changeset.Writes(file.DestPath) {
		return nil
	}
	return checkNextConfig(units, file, unit, changeset)
}

// checkNextUnit runs the checks of the Agent's next unit: its sources
// (checkLostSources), and, when the changeset writes it over a unit on disk,
// its frozen lines (checkFrozenLines).
func checkNextUnit(units agentUnits, unit string, rootful bool, changeset *reconciler.Changeset) error {
	if err := checkLostSources(units.next, unit, changeset); err != nil {
		return err
	}
	if !units.writes || units.running == nil {
		return nil
	}
	return checkFrozenLines(units.running, units.next, unit, units.diskPath, rootful)
}

// checkAgentServiceKept refuses, when no resolved quadlet generates unit (the
// Agent's service), a changeset after which the Agent would run on from a
// unit the guard never checked: one that updates the container quadlet the
// state records as generating unit (its new content generates another
// service: ServiceName=, or a rename), or deletes it while the Agent bundle
// stays assigned (the bundle renders the Agent's container under another
// name). A changeset that drops the Agent bundle decommissions the Fleet's
// Agent unit.
func checkAgentServiceKept(resolved *resolver.ResolvedHost, unit string, changeset *reconciler.Changeset) error {
	bundle := config.AgentService(resolved.Host)
	assigned := slices.Contains(resolved.Services, bundle)
	for _, change := range changeset.Changes {
		if change.Category != config.CategoryContainer || change.OldServiceName != unit {
			continue
		}
		switch {
		case change.Action == reconciler.ActionUpdate:
			return fmt.Errorf("refusing %s: the Agent's service %s runs from it, and this commit makes it generate %s instead, "+
				"so the Agent would run on from a unit the self-restart guard never checked: keep the Agent's quadlet generating %s, "+
				"or unassign the Agent bundle %s to stop managing the Agent", change.DestPath, unit, change.ServiceName, unit, bundle)
		case change.Action == reconciler.ActionDelete && assigned:
			return fmt.Errorf("refusing to remove %s: the Agent's service %s runs from it, and the Agent bundle %s stays assigned "+
				"with no quadlet generating that service, so the Agent would run on from a unit the self-restart guard never checked: "+
				"keep the Agent's quadlet generating %s, or unassign the bundle to stop managing the Agent", change.DestPath, unit, bundle, unit)
		}
	}
	return nil
}

// checkOneAgentService refuses every resolved file but quadlet, the Agent's,
// that would also be the Agent's service unit: a quadlet that generates it
// (Quadlet writes the service once per quadlet, and the last one wins), a
// raw unit file of its name (systemd ranks its unit directory above
// Quadlet's generator directory), or a unit whose [Install] Alias= names it
// (enabling it links the alias into the unit directory). The guard checks
// only quadlet, so the Agent's next start must come from it.
func checkOneAgentService(files []resolver.ResolvedFile, quadlet *resolver.ResolvedFile, unit string) error {
	for i := range files {
		file := &files[i]
		var how string
		switch {
		case file == quadlet:
			continue
		case file.ServiceName == unit && file.Category == config.CategorySystemd:
			how = "it is"
		case file.ServiceName == unit:
			how = "it generates"
		case slices.Contains(installAliases(file), unit):
			how = "its [Install] Alias= names"
		default:
			continue
		}
		return fmt.Errorf("refusing %s: %s the Agent's service %s, which %s generates; the Agent must start from one unit only, "+
			"the quadlet the self-restart guard checks: remove the other unit or give it another name", file.DestPath, how, unit, quadlet.DestPath)
	}
	return nil
}

// installAliases returns the [Install] Alias= names of a resolved quadlet or
// raw systemd unit; none for a raw unit that does not parse (the validator
// reports it).
func installAliases(file *resolver.ResolvedFile) []string {
	unit := file.ParsedUnit
	if unit == nil && file.Category == config.CategorySystemd {
		unit = parser.NewUnitFile()
		if unit.Parse(file.Content) != nil {
			return nil
		}
	}
	if unit == nil {
		return nil
	}
	return unit.LookupAllStrv("Install", "Alias")
}

// checkNextConfig checks file, the config the Agent's next start reads with
// units.next: when the changeset writes the Agent's unit and there is none on
// disk, whose mounts are unknown, its credentials are read where they are
// spelled, as at the Agent's startup; otherwise, with the mounts the next
// start keeps, in the running container too, where a fresh start would show
// them alike (sameView).
func checkNextConfig(units agentUnits, file *resolver.ResolvedFile, unit string, changeset *reconciler.Changeset) error {
	locate := absolutePath
	switch {
	case units.running != nil:
		locate = sameView(units.running, units.next, changeset)
	case !units.writes:
		locate = sameView(units.next, units.next, changeset)
	}
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content), locate); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// agentUnits are the Agent units a changeset is checked against.
type agentUnits struct {
	// running is the unit on disk the running container was started from
	// (runningUnit), at diskPath; nil when there is none.
	running  *parser.UnitFile
	diskPath string
	// next is the unit the Agent's next start uses: the Fleet's render when
	// the changeset writes the Agent's quadlet (writes), else the unit on
	// disk (else the render), which the apply leaves as it is, also with a
	// Host-side edit the Fleet has not committed yet.
	next   *parser.UnitFile
	writes bool
}

// agentUnitsOf returns the agentUnits of the Agent's resolved quadlet.
func agentUnitsOf(quadlet *resolver.ResolvedFile, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (agentUnits, error) {
	running, diskPath, err := runningUnit(quadlet, changeset, readFile)
	if err != nil {
		return agentUnits{}, err
	}
	units := agentUnits{running: running, diskPath: diskPath, next: quadlet.ParsedUnit, writes: changeset.Writes(quadlet.DestPath)}
	if !units.writes && running != nil {
		units.next = running
	}
	return units, nil
}

// agentConfigFile returns the resolved secret the unit next mounts where the
// Agent reads its config (configPath), by Secret= or PodmanArgs= --secret
// (the last one there); nil when no Fleet-resolved secret is mounted there:
// a Host path, a volume, an unresolved secret or a path the guard cannot
// tell is a config the Fleet does not render.
func agentConfigFile(files []resolver.ResolvedFile, next *parser.UnitFile) *resolver.ResolvedFile {
	target := path.Clean(configPath(next))
	mounts, _ := unitMounts(next)
	var name string
	for _, m := range mounts {
		if m.secret != "" && m.target == target {
			name = m.secret
		}
	}
	for i := range files {
		if name != "" && files[i].Category == config.CategorySecret && files[i].DestPath == resolver.SecretDestPath(name) {
			return &files[i]
		}
	}
	return nil
}

// frozenKeys are the Agent unit's [Container] keys the Fleet may not change
// (checkFrozenLines): every key that decides what paths the container shows
// (mounts, masks, the root file system, the working directory), who reads
// them (user, groups, user namespace and ID maps, capabilities, AppArmor and
// SELinux labels), what the Agent runs and where it finds its config (Exec=,
// Entrypoint=, environment files and the Host's environment), or what podman
// adds to any of them (PodmanArgs=, GlobalArgs=, ContainersConfModule=).
// AddDevice= places a device at any container path, and Annotation= sets
// OCI runtime options such as crun's run.oci.keep_original_groups (the
// GroupAdd=keep-groups of crun). Image= is not frozen: the Agent updates
// itself.
var frozenKeys = []string{
	"Volume", "Mount", "Tmpfs", "Secret", "Mask", "Unmask", "VolatileTmp",
	"ReadOnly", "ReadOnlyTmpfs", "Rootfs", "WorkingDir", "AddDevice", "Annotation",
	"User", "Group", "GroupAdd", "UserNS", "UIDMap", "GIDMap", "SubUIDMap", "SubGIDMap",
	"RemapUsers", "RemapUid", "RemapGid", "RemapUidSize",
	"AddCapability", "DropCapability", "AppArmor", "SeccompProfile",
	"SecurityLabelDisable", "SecurityLabelFileType", "SecurityLabelLevel", "SecurityLabelNested", "SecurityLabelType",
	"Exec", "Entrypoint", "EnvironmentFile", "EnvironmentHost",
	"PodmanArgs", "GlobalArgs", "ContainersConfModule",
}

// changeableServiceKeys are the only [Service] keys of the Agent unit the
// Fleet may change: every other one is frozen (checkFrozenLines), since
// [Service] decides who runs podman (User=, Group=), its environment
// (Environment= and the like, which systemd also expands in the generated
// ExecStart, and CONTAINERS_CONF_OVERRIDE), and what runs around it.
var changeableServiceKeys = []string{"Restart", "RestartSec", "TimeoutStartSec", "TimeoutStopSec"}

// checkFrozenLines refuses a next Agent unit whose frozen lines differ from
// those of running, the unit on disk (at diskPath) the running container was
// started from (frozenChanges). The guard checks the next start in the
// running container, so it could not check one with other mounts, another
// identity or another config path; the Host's operator changes them by hand
// and restarts the Agent first (Quadlet regenerates the service on
// daemon-reload; rootful: the Host's system manager), and the Fleet then
// commits the same lines.
func checkFrozenLines(running, next *parser.UnitFile, unit, diskPath string, rootful bool) error {
	changed := frozenChanges(running, next)
	if len(changed) == 0 {
		return nil
	}
	restart := "systemctl --user daemon-reload && systemctl --user restart " + unit + ", as the Host's user"
	if rootful {
		restart = "systemctl daemon-reload && systemctl restart " + unit
	}
	return fmt.Errorf("refusing the Agent unit %s: this commit changes its %s lines, and the Agent unit's mounts can't change through the Fleet: "+
		"edit %s on the Host to the new lines, restart the Agent (%s), then commit the same lines",
		unit, strings.Join(changed, ", "), diskPath, restart)
}

// frozenChanges names the frozen lines next changes from running, compared
// as written (each key's values, trimmed, in order): the frozenKeys of
// [Container], the PICOLET_CONFIG its Environment= lines set (configPath),
// and every [Service] key but changeableServiceKeys.
func frozenChanges(running, next *parser.UnitFile) []string {
	var changed []string
	for _, key := range frozenKeys {
		if !slices.Equal(trimmedValues(running, "Container", key), trimmedValues(next, "Container", key)) {
			changed = append(changed, key+"=")
		}
	}
	if configPath(running) != configPath(next) {
		changed = append(changed, "Environment=PICOLET_CONFIG")
	}
	keys := slices.Concat(running.ListKeys("Service"), next.ListKeys("Service"))
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		if !slices.Contains(changeableServiceKeys, key) &&
			!slices.Equal(trimmedValues(running, "Service", key), trimmedValues(next, "Service", key)) {
			changed = append(changed, "[Service] "+key+"=")
		}
	}
	return changed
}

// configPath is where the Agent in unit's container reads its config:
// PICOLET_CONFIG as unit's [Container] Environment= lines set it, read as
// Quadlet reads them, or the default, resolver.AgentConfigTarget. A
// PICOLET_CONFIG without a value takes podman's own ("$PICOLET_CONFIG").
func configPath(unit *parser.UnitFile) string {
	env, _ := unit.LookupAllKeyVal("Container", "Environment") // a value Quadlet cannot split sets nothing
	value, ok := env["PICOLET_CONFIG"]
	switch {
	case !ok:
		return resolver.AgentConfigTarget
	case value == nil:
		return "$PICOLET_CONFIG"
	}
	return *value
}

// trimmedValues returns unit's values of key in group, each trimmed.
func trimmedValues(unit *parser.UnitFile, group, key string) []string {
	values := unit.LookupAll(group, key)
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
// container (started from running), which the next start, with the unit
// next and the same mounts (checkFrozenLines), replaces: a credential path
// must be absolute, and a file credential is refused where a fresh start may
// show it otherwise than the running container: when next passes
// --volumes-from (the guard cannot read what it mounts), under a mount that
// may hold it and that a fresh start fills anew (freshMount), or under no
// mount at all, in the image, when the changeset changes Image=. The mounts
// that may hold a path are those at the deepest target holding it and every
// mount whose target the guard cannot place (relative or specifier-led). A
// directory credential (protonpass.session_dir) need not exist, so it need
// only be absolute.
func sameView(running, next *parser.UnitFile, changeset *reconciler.Changeset) agentcfg.Locate {
	mounts, volumesFrom := unitMounts(next)
	imageChanged := !slices.Equal(trimmedValues(running, "Container", "Image"), trimmedValues(next, "Container", "Image"))
	return func(cred agentcfg.Credential) (string, error) {
		if _, err := absolutePath(cred); err != nil || cred.Dir {
			return cred.Path, err
		}
		if len(volumesFrom) > 0 {
			return "", fmt.Errorf("the Agent unit passes %s in PodmanArgs=, which mounts what the guard cannot read; "+
				"keep credentials on a Host path or in a secret", strings.Join(volumesFrom, ", "))
		}
		held := mayHold(mounts, cred.Path)
		if imageChanged && !slices.ContainsFunc(held, placed) {
			return "", errors.New("it lies under no mount of the Agent unit, so the next start reads it from the image, which this commit changes (Image=); " +
				"keep credentials on a Host path or in a secret")
		}
		for _, m := range held {
			if err := freshMount(m, changeset); err != nil {
				return "", err
			}
		}
		return cred.Path, nil
	}
}

// freshMount refuses a mount m of the Agent unit that may hold a credential
// and that a fresh start fills otherwise than the running container shows
// it: a tmpfs or anonymous volume (Quadlet runs the container with --rm, so
// it starts empty), an image (a pull or rebuild may change it), or a
// Fleet-managed secret or .volume unit the changeset writes (the running
// container still shows the old content).
func freshMount(m mount, changeset *reconciler.Changeset) error {
	switch {
	case m.empty:
		return fmt.Errorf("the Agent unit mounts a tmpfs or anonymous volume at %s, which may hold it and starts empty in a new container; "+
			"keep credentials on a Host path or in a secret", m.target)
	case m.image:
		return fmt.Errorf("the Agent unit mounts an image at %s, which may hold it and whose content a pull or rebuild may change; "+
			"keep credentials on a Host path or in a secret", m.target)
	case m.secret != "" && changeset.Writes(resolver.SecretDestPath(m.secret)):
		return changedSource("the secret " + m.secret)
	case m.volumeUnit != "" && writesVolumeUnit(changeset, m.volumeUnit):
		return changedSource("the volume unit " + m.volumeUnit)
	}
	return nil
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
// the file the state records as generating the Agent's service
// (Change.OldServiceName), which the changeset leaves, updates (whatever
// service its new content names) or deletes (a rename creates a new file and
// deletes the old one), else the file at quadlet's path when the state does
// not record it (a reset state, or a hand-placed quadlet the Fleet takes
// over), and returns it with its path. It returns nil when there is none.
func runningUnit(quadlet *resolver.ResolvedFile, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) (*parser.UnitFile, string, error) {
	for _, diskPath := range runningUnitPaths(quadlet, changeset) {
		data, readErr := readFile(diskPath)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, "", fmt.Errorf("reading the running Agent's unit %s: %w", diskPath, readErr)
		}
		running := parser.NewUnitFile()
		running.Filename = path.Base(diskPath)
		if err := running.Parse(string(data)); err != nil {
			return nil, "", fmt.Errorf("parsing the running Agent's unit %s: %w", diskPath, err)
		}
		return running, diskPath, nil
	}
	return nil, "", nil
}

// runningUnitPaths returns the paths runningUnit tries, in order: the
// container quadlets the state records as generating quadlet's service, then
// quadlet's own path when the state does not record it.
func runningUnitPaths(quadlet *resolver.ResolvedFile, changeset *reconciler.Changeset) []string {
	var recorded []string
	var created []string
	for _, change := range changeset.Changes {
		switch {
		case change.Category != config.CategoryContainer:
		case change.Action != reconciler.ActionCreate && change.OldServiceName == quadlet.ServiceName:
			recorded = append(recorded, change.DestPath)
		case change.Action == reconciler.ActionCreate && change.DestPath == quadlet.DestPath:
			created = append(created, change.DestPath)
		}
	}
	return append(recorded, created...)
}

// selfConfigRestart returns the Applier option that restarts the Agent's
// unit after an apply that writes the Agent's Fleet-rendered config (the
// secret its next unit mounts where it reads its config, agentConfigFile):
// the running Agent never reloads its config, and Podman copies the secret
// into the container only when it creates it. CheckSelfConfig has already
// checked that config, so the restart cannot lock the Agent out. It returns
// none otherwise.
func selfConfigRestart(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) []applier.Option {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	quadlet := agentQuadlet(resolved.Files, unit)
	if quadlet == nil {
		return nil
	}
	units, err := agentUnitsOf(quadlet, changeset, readFile)
	if err != nil {
		return nil // CheckSelfConfig read the same unit and passed
	}
	file := agentConfigFile(resolved.Files, units.next)
	if file == nil || !changeset.Writes(file.DestPath) {
		return nil
	}
	return []applier.Option{applier.WithSelfConfig(unit, reconciler.SecretNameFromPath(file.DestPath))}
}
