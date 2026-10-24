package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The default Service Bundles of a Host's Agent, one per systemd instance.
const (
	// agentServiceUser is the Agent bundle of a Host with user:.
	agentServiceUser = "picolet"
	// agentServiceSystem is the Agent bundle of the rootful Host.
	agentServiceSystem = "picolet-system"
)

// ErrAgentNotAssigned marks a Host whose Agent bundle is not assigned to it.
var ErrAgentNotAssigned = errors.New("agent bundle not assigned")

// DefaultAgentService is the Agent bundle of a systemd instance: the user
// instance's for userInstance, the system instance's otherwise.
func DefaultAgentService(userInstance bool) string {
	if userInstance {
		return agentServiceUser
	}
	return agentServiceSystem
}

// AgentService is the Service Bundle of host's Agent as the Fleet names it:
// picolet for a Host with user:, picolet-system for the rootful Host. The
// bundle need not be assigned.
func AgentService(host *HostConfig) string {
	return DefaultAgentService(!host.Rootful())
}

// AssignedAgentService is AgentService(host) when the Host's assignments
// include it, the check bootstrap's Agent resolution applies. Otherwise the
// error wraps ErrAgentNotAssigned.
func (c *Config) AssignedAgentService(host *HostConfig) (string, error) {
	service := AgentService(host)
	if assigned := c.Assignments.Resolve(host).Services; !slices.Contains(assigned, service) {
		return "", fmt.Errorf("host %s: %w: %s, the Agent of a %s, is not among the assigned services (%s)",
			host.Hostname, ErrAgentNotAssigned, service, agentKind(host), strings.Join(assigned, ", "))
	}
	return service, nil
}

func agentKind(host *HostConfig) string {
	if host.Rootful() {
		return "rootful Host"
	}
	return "Host with user:"
}
