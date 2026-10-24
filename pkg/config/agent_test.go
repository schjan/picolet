package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssignedAgentService(t *testing.T) {
	t.Parallel()
	cfg := &Config{Assignments: &Assignments{Roles: map[string]AssignmentGroup{
		"node": {Services: []string{"picolet", "picolet-system"}},
	}}}
	tests := []struct {
		name string
		host *HostConfig
		want string
	}{
		{name: "rootful Host", host: &HostConfig{Hostname: "a", Role: "node"}, want: "picolet-system"},
		{name: "Host with user:", host: &HostConfig{Hostname: "b", Role: "node", User: "app"}, want: "picolet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cfg.AssignedAgentService(tt.host)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// An unassigned Agent bundle wraps ErrAgentNotAssigned and names the Host,
// the missing bundle and what is assigned.
func TestAssignedAgentServiceNotAssigned(t *testing.T) {
	t.Parallel()
	cfg := &Config{Assignments: &Assignments{Roles: map[string]AssignmentGroup{
		"node": {Services: []string{"web"}},
	}}}
	_, err := cfg.AssignedAgentService(&HostConfig{Hostname: "node-1", Role: "node"})
	require.ErrorIs(t, err, ErrAgentNotAssigned)
	require.EqualError(t, err, "host node-1: agent bundle not assigned: picolet-system, the Agent of a rootful Host, is not among the assigned services (web)")
}
