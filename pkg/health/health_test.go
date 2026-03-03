package health

import (
	"context"
	"testing"
	"time"

	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/state"
)

func TestEnforceAllHealthy(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	sys.EXPECT().IsActive(mock.Anything, mock.MatchedBy(func(s string) bool {
		return s == "foo.service" || s == "bar-network.service"
	})).Return(true, nil).Times(2)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
			"/etc/containers/systemd/bar.network":   "sha256:def",
			"secret:my_secret":                      "sha256:ghi", // skipped
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Healthy, 2)
	assert.Empty(t, result.Unhealthy)
}

func TestEnforceRestartsUnhealthy(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	sys.EXPECT().IsActive(mock.Anything, "foo.service").Return(false, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(nil)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Unhealthy, 1)
}

func TestEnforceSkipsSecretsAndManifests(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	// No expectations — no units should be checked
	c := New(sys)

	st := &state.State{
		ManagedFiles: map[string]string{
			"secret:my_secret": "sha256:abc",
			"/var/lib/picolet/manifests/app/deployment.yml": "sha256:def",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Empty(t, result.Healthy)
	assert.Empty(t, result.Unhealthy)
}

func TestEnforceHandlesCheckError(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	sys.EXPECT().IsActive(mock.Anything, "foo.service").Return(false, assert.AnError)

	c := New(sys)
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Errors, 1)
}

func TestEnforceRestartCooldown(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	// IsActive called twice (two Enforce calls), unit is unhealthy both times
	sys.EXPECT().IsActive(mock.Anything, "foo.service").Return(false, nil).Times(2)
	// RestartUnit should only be called once (cooldown prevents second restart)
	sys.EXPECT().RestartUnit(mock.Anything, "foo.service").Return(nil).Once()

	c := New(sys)
	// Override cooldown to a large value so second call is within cooldown
	c.lastRestart["foo.service"] = time.Time{} // ensure clean state

	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:abc",
		},
	}

	// First enforce: restarts
	result1, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result1.Unhealthy, 1)

	// Second enforce immediately: cooldown prevents restart
	result2, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result2.Unhealthy, 1)
	assert.Empty(t, result2.Errors)
}
