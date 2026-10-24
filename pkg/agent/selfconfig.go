package agent

import (
	"fmt"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
)

// checkSelfConfig refuses a changeset that would change the Agent's own
// Fleet-rendered config to one the Agent could not start with: one its
// strict loader rejects or one whose startup credential checks fail
// (agentcfg.CheckRendered). A changed config does not restart the Agent, so
// applying it would only lock the Agent out at its next restart, when it can
// no longer fetch a fix.
//
// The config is the secret the Host's Agent bundle mounts at the Agent config
// path (resolver.AgentConfigSecret). A Host whose Agent config is not
// Fleet-rendered (no Agent bundle, or no such mount) has nothing to check.
func checkSelfConfig(resolved *resolver.ResolvedHost, changeset *reconciler.Changeset) error {
	if resolved.Host == nil {
		return nil
	}
	unit := config.AgentService(resolved.Host) + ".service"
	file, err := resolver.AgentConfigSecret(resolved.Files, unit)
	if err != nil {
		return nil //nolint:nilerr // not Fleet-rendered: nothing to guard
	}
	if !changesFile(changeset, file.DestPath) {
		return nil
	}
	if err := agentcfg.CheckRendered([]byte(file.Content)); err != nil {
		return fmt.Errorf("refusing the Agent config %s mounted by %s: the Agent could not start with it: %w", file.DestPath, unit, err)
	}
	return nil
}

// changesFile reports whether changeset creates or updates destPath.
func changesFile(changeset *reconciler.Changeset, destPath string) bool {
	for _, change := range changeset.Changes {
		if change.DestPath == destPath && (change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate) {
			return true
		}
	}
	return false
}
