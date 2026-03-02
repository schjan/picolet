package health

import (
	"context"
	"fmt"
	"testing"

	"github.com/schjan/picolet/pkg/state"
)

type mockSystemd struct {
	states    map[string]string
	restarted []string
	failCheck map[string]bool
}

func (m *mockSystemd) DaemonReload(context.Context) error          { return nil }
func (m *mockSystemd) StartUnit(_ context.Context, _ string) error { return nil }

func (m *mockSystemd) RestartUnit(_ context.Context, name string) error {
	m.restarted = append(m.restarted, name)
	return nil
}

func (m *mockSystemd) GetUnitState(_ context.Context, name string) (string, error) {
	if m.failCheck[name] {
		return "", fmt.Errorf("dbus error for %s", name)
	}
	s, ok := m.states[name]
	if !ok {
		return "inactive", nil
	}
	return s, nil
}

func (m *mockSystemd) IsActive(_ context.Context, name string) (bool, error) {
	if m.failCheck[name] {
		return false, fmt.Errorf("dbus error for %s", name)
	}
	s, ok := m.states[name]
	if !ok {
		return false, nil
	}
	return s == "active", nil
}

func TestEnforceAllHealthy(t *testing.T) {
	sys := &mockSystemd{
		states: map[string]string{
			"foo.service":         "active",
			"bar-network.service": "active",
		},
	}
	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
			"/etc/containers/systemd/bar.network":   "sha256:def",
			"secret:my_secret":                      "sha256:ghi", // skipped
		},
	}

	result, err := c.Enforce(context.Background(), st)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(result.Healthy) != 2 {
		t.Errorf("Healthy = %d, want 2", len(result.Healthy))
	}
	if len(result.Unhealthy) != 0 {
		t.Errorf("Unhealthy = %d, want 0", len(result.Unhealthy))
	}
	if len(sys.restarted) != 0 {
		t.Errorf("restarted = %v, want none", sys.restarted)
	}
}

func TestEnforceRestartsUnhealthy(t *testing.T) {
	sys := &mockSystemd{
		states: map[string]string{
			"foo.service": "failed",
		},
	}
	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(result.Unhealthy) != 1 {
		t.Errorf("Unhealthy = %d, want 1", len(result.Unhealthy))
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "foo.service" {
		t.Errorf("restarted = %v, want [foo.service]", sys.restarted)
	}
}

func TestEnforceSkipsSecretsAndManifests(t *testing.T) {
	sys := &mockSystemd{states: make(map[string]string)}
	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]string{
			"secret:my_secret": "sha256:abc",
			"/var/lib/picolet/manifests/app/deployment.yml": "sha256:def",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(result.Healthy)+len(result.Unhealthy) != 0 {
		t.Error("expected no unit checks for secrets/manifests")
	}
}

func TestEnforceHandlesCheckError(t *testing.T) {
	sys := &mockSystemd{
		states:    make(map[string]string),
		failCheck: map[string]bool{"foo.service": true},
	}
	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Errorf("Errors = %d, want 1", len(result.Errors))
	}
}
