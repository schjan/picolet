package agent

import (
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// checkSelfConfig is the self-restart guard: it refuses a changeset that
// would change the Agent's own Fleet-rendered config to one the Agent could
// not start with: one its strict loader rejects or one whose startup
// credential checks fail (agentcfg.CheckRenderedConfig). A changed config does not restart the Agent, so
// applying it would only lock the Agent out at its next restart, when it can
// no longer fetch a fix.
//
// The config is the secret the Host's Agent bundle mounts at the Agent config
// path (resolver.AgentConfigSecret). A Host whose Agent config is not
// Fleet-rendered (no Agent bundle, or no such mount) has nothing to check.
// The check runs when the changeset writes that secret or the Agent's own
// unit file: a unit change can repoint the mount at another managed secret
// that is itself unchanged, and the Agent's next start reads that one.
func checkSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finding the Agent config mounted by %s: %w", unit, err)
	}
	if !changeset.Writes(file.DestPath) && !writesUnit(resolved.Files, unit, changeset) {
		return nil
	}
	if err := agentcfg.CheckRenderedConfig([]byte(file.Content)); err != nil {
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
