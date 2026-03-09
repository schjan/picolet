package health

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
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
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
			"/etc/containers/systemd/bar.network":   {Hash: "sha256:def", Category: "network"},
			"secret:my_secret":                      {Hash: "sha256:ghi", Category: "secret"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
			"/etc/containers/systemd/bar.network":   "bar-network.service",
			// secret has no entry — skipped automatically
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
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	result, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result.Unhealthy, 1)
	assert.Equal(t, []string{"foo.service"}, result.Restarted)
	assert.Empty(t, result.Skipped)
}

func TestEnforceSkipsSecretsAndManifests(t *testing.T) {
	t.Parallel()
	sys := mocks.NewMockSystemdManager(t)
	// No expectations — no units should be checked
	c := New(sys)

	st := state.NewState()
	st.ManagedFiles["secret:my_secret"] = state.ManagedFile{Hash: "sha256:abc", Category: "secret"}
	st.ManagedFiles["/var/lib/picolet/manifests/app/deployment.yml"] = state.ManagedFile{Hash: "sha256:def", Category: "manifest"}
	// no quadlet units → ServiceNames stays empty

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
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
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

	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/foo.container": "foo.service",
		},
	}

	// First enforce: restarts
	result1, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result1.Unhealthy, 1)
	assert.Equal(t, []string{"foo.service"}, result1.Restarted)
	assert.Empty(t, result1.Skipped)

	// Second enforce immediately: cooldown prevents restart
	result2, err := c.Enforce(context.Background(), st)
	require.NoError(t, err)
	assert.Len(t, result2.Unhealthy, 1)
	assert.Empty(t, result2.Errors)
	assert.Empty(t, result2.Restarted)
	assert.Equal(t, []string{"foo.service"}, result2.Skipped)
}
