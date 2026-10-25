package agent

import (
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// CheckSelfConfig is the self-restart guard: it refuses a changeset that
// would leave the Agent's own Fleet-rendered config one the Agent could not
// start with: one its strict loader rejects or one whose startup credential
// checks fail (agentcfg.CheckRenderedConfig), or a removed one. A changed
// config does not restart the Agent, so applying it would only lock the
// Agent out at its next restart, when it can no longer fetch a fix. The
// Agent runs it before every apply, and so does `picolet apply --config`.
//
// The config is the secret the Host's Agent bundle mounts at the Agent config
// path (resolver.AgentConfigSecret). A Host whose Agent config is not
// Fleet-rendered (no Agent bundle, no such mount, or a mounted secret the
// Fleet never managed) has nothing to check. A changeset that removes the
// mounted secret, because the Fleet no longer resolves it (its file renamed
// or dropped), is refused. Otherwise the check runs when the changeset writes
// that secret or the Agent's own unit file: a unit change can repoint the
// mount at another managed secret that is itself unchanged, and the Agent's
// next start reads that one. When the changeset writes the unit, the
// credential files are looked up through the new unit's mounts
// (selfConfigLocator, reading the running Agent's unit with readFile).
func CheckSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset, readFile func(string) ([]byte, error)) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	name, ok := resolver.AgentConfigMount(resolved.Files, unit)
	if !ok {
		return nil
	}
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return checkDroppedSelfConfig(name, unit, changeset)
	}
	if err != nil {
		return fmt.Errorf("finding the Agent config mounted by %s: %w", unit, err)
	}
	if !changeset.Writes(file.DestPath) && !writesUnit(resolved.Files, unit, changeset) {
		return nil
	}
	locate, err := selfConfigLocator(resolved.Files, unit, changeset, readFile)
	if err != nil {
		return err
	}
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content), locate); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// writesUnit reports whether the changeset writes the file of the unit named
// unit (its Quadlet file).
func writesUnit(files []resolver.ResolvedFile, unit string, changeset *reconciler.Changeset) bool {
	for _, file := range files {
		if file.ServiceName == unit && changeset.Writes(file.DestPath) {
			return true
		}
	}
	return false
}

// checkDroppedSelfConfig refuses a changeset that removes the secret named
// name, which unit still mounts at the Agent config path but the Fleet no
// longer resolves (its file was renamed or dropped): the Agent's next start
// would find no config. A mounted secret the Fleet never managed is not
// Fleet-rendered and has nothing to check.
func checkDroppedSelfConfig(name, unit string, changeset *reconciler.Changeset) error {
	if !changeset.Deletes("secret:" + name) {
		return nil
	}
	return fmt.Errorf("refusing to remove the Agent config secret %s: %s still mounts it at %s, so the Agent could not start; "+
		"keep resolving the secret, or change the mount in the same commit", name, unit, resolver.AgentConfigTarget)
}
