package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The default Service Bundles of a Host's Agent, one per systemd instance.
const (
	// AgentServiceUser is the Agent bundle of a Host with user:.
	AgentServiceUser = "picolet"
	// AgentServiceSystem is the Agent bundle of the rootful Host.
	AgentServiceSystem = "picolet-system"
)

// ErrAgentNotAssigned marks a Host whose Agent bundle is not assigned to it.
var ErrAgentNotAssigned = errors.New("agent bundle not assigned")

// DefaultAgentService is the Agent bundle of a systemd instance: the user
// instance's for userInstance, the system instance's otherwise.
func DefaultAgentService(userInstance bool) string {
	if userInstance {
		return AgentServiceUser
	}
	return AgentServiceSystem
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
		return "", notAssignedError{msg: fmt.Sprintf("host %s has no %s service assigned, the Agent of a %s (assigned: %s)",
			host.Hostname, service, agentKind(host), strings.Join(assigned, ", "))}
	}
	return service, nil
}

// notAssignedError is ErrAgentNotAssigned with its Host's message, which
// reads without the sentinel's text.
type notAssignedError struct{ msg string }

func (e notAssignedError) Error() string        { return e.msg }
func (e notAssignedError) Is(target error) bool { return target == ErrAgentNotAssigned }

func agentKind(host *HostConfig) string {
	if host.Rootful() {
		return "rootful Host"
	}
	return "Host with user:"
}
