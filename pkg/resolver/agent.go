package resolver

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"

	"github.com/schjan/picolet/pkg/config"
)

// AgentConfigTarget is the in-container path the Agent reads its config
// from; the Agent bundle's container quadlet mounts the config secret there.
const AgentConfigTarget = "/etc/picolet/config.yml"

// ErrAgentConfigNotRendered is wrapped by AgentConfigSecret when the Agent's
// config is not Fleet-rendered: the unit declares no Secret= mount at
// AgentConfigTarget, or the secret it mounts is not one the Fleet resolves.
var ErrAgentConfigNotRendered = errors.New("agent config is not Fleet-rendered")

// SecretDestPath is the DestPath of the Podman secret named name.
func SecretDestPath(name string) string { return "secret:" + name }

// AgentConfigSecret returns the resolved secret the Agent bundle's container
// quadlet (the one generating unitName) mounts at the Agent config path
// (Secret=<name>,target=/etc/picolet/config.yml).
func AgentConfigSecret(files []ResolvedFile, unitName string) (*ResolvedFile, error) {
	name, ok := AgentConfigMount(files, unitName)
	if !ok {
		return nil, fmt.Errorf("%w: %s declares no Secret=...,target=%s mount for the agent config", ErrAgentConfigNotRendered, unitName, AgentConfigTarget)
	}
	for i := range files {
		if files[i].Category == config.CategorySecret && files[i].DestPath == SecretDestPath(name) {
			return &files[i], nil
		}
	}
	return nil, fmt.Errorf("%w: config secret %s referenced by %s was not resolved by the bundle", ErrAgentConfigNotRendered, name, unitName)
}

// AgentConfigMount returns the name of the secret the quadlet generating
// unitName mounts at the Agent config path, whether or not the Fleet
// resolves that secret; ok is false when no resolved quadlet declares such a
// mount.
func AgentConfigMount(files []ResolvedFile, unitName string) (name string, ok bool) {
	for _, file := range files {
		if file.ServiceName != unitName || file.ParsedUnit == nil {
			continue
		}
		if n, found := AgentConfigMountOf(file.ParsedUnit); found {
			name, ok = n, true
		}
	}
	return name, ok
}

// AgentConfigMountOf returns the name of the secret unit's [Container] group
// mounts at the Agent config path, however the target is spelled (see
// ParseSecretMount); the last such Secret= wins.
func AgentConfigMountOf(unit *parser.UnitFile) (name string, ok bool) {
	for _, spec := range unit.LookupAll("Container", "Secret") {
		if n, target, mounted := ParseSecretMount(spec); mounted && target == AgentConfigTarget {
			name, ok = n, true
		}
	}
	return name, ok
}

// ParseSecretMount parses a quadlet Secret= value (name[,type=…][,target=…])
// as Podman mounts it: a mount secret (type=mount, the default) is mounted at
// its target, by default /run/secrets/<name>, and a relative target is under
// /run/secrets. target is cleaned; mounted is false for an env secret.
func ParseSecretMount(spec string) (name, target string, mounted bool) {
	fields := strings.Split(spec, ",")
	name = fields[0]
	target = name
	for _, field := range fields[1:] {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "type":
			if value != "mount" {
				return name, "", false
			}
		case "target":
			target = value
		}
	}
	if !path.IsAbs(target) {
		target = path.Join("/run/secrets", target)
	}
	return name, path.Clean(target), true
}
