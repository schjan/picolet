package resolver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// AgentConfigTarget is the in-container path the Agent reads its config
// from; the Agent bundle's container quadlet mounts the config secret there.
const AgentConfigTarget = "/etc/picolet/config.yml"

// AgentConfigSecret returns the resolved secret the Agent bundle's container
// quadlet (the one generating unitName) mounts at the Agent config path
// (Secret=<name>,target=/etc/picolet/config.yml).
func AgentConfigSecret(files []ResolvedFile, unitName string) (*ResolvedFile, error) {
	name := ""
	for _, file := range files {
		if file.ServiceName != unitName || file.ParsedUnit == nil {
			continue
		}
		for _, secret := range file.ParsedUnit.LookupAll("Container", "Secret") {
			parts := strings.Split(secret, ",")
			if slices.Contains(parts[1:], "target="+AgentConfigTarget) {
				name = parts[0]
			}
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%s declares no Secret=...,target=%s mount for the agent config", unitName, AgentConfigTarget)
	}
	for i := range files {
		if files[i].Category == config.CategorySecret && files[i].DestPath == "secret:"+name {
			return &files[i], nil
		}
	}
	return nil, fmt.Errorf("config secret %s referenced by %s was not resolved by the bundle", name, unitName)
}
