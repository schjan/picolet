package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/state"
)

const exampleFleet = "../../testdata/example-fleet"

func TestResolveAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want Agent
	}{
		{host: "vps-1", want: Agent{
			Service: "picolet", Unit: "picolet.service", Image: "ghcr.io/schjan/picolet:v0.2.0", DialAddr: "127.0.0.1:9417",
		}},
		// The rootful Agent binds 0.0.0.0, which a probe cannot dial.
		{host: "vps-1-system", want: Agent{
			Service: "picolet-system", Unit: "picolet-system.service", Image: "ghcr.io/schjan/picolet:v0.2.0", DialAddr: "127.0.0.1:9418",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveAgent(t.Context(), exampleFleet, tt.host)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveAgentWithoutAgentBundle(t *testing.T) {
	t.Parallel()
	_, err := ResolveAgent(t.Context(), exampleFleet, "node-1")
	require.ErrorContains(t, err, "host node-1 has no picolet-system service assigned")
}

// Missing inputs are reported in the bootstrap's order, hostname before repo
// dir, before the Fleet is opened.
func TestMissingInputsReportedInOrder(t *testing.T) {
	t.Parallel()
	_, err := ResolveAgent(t.Context(), "", "")
	require.EqualError(t, err, "hostname is required")
	_, err = prepare(t.Context(), RunConfig{})
	require.EqualError(t, err, "hostname is required")
	_, err = prepare(t.Context(), RunConfig{Hostname: "vps-1"})
	require.EqualError(t, err, "repo dir is required")
}

// The per-Host bootstrap run as `bootstrap machine` starts it in the Host's
// container (no --rootless, no --data-dir) seeds state at the paths the Agent
// sees inside its own container, never below a home: the Agent's first
// Reconciliation then finds its own quadlet already managed.
//
// An intentional exception to testing through public seams: the issue's
// acceptance criterion asks for the seeded keys, which no public seam shows
// without systemd and Podman. prepare and changeset are what Run itself
// builds the saved state from.
func TestHostBootstrapSeedsContainerInternalStateKeys(t *testing.T) {
	t.Parallel()
	p, err := prepare(t.Context(), RunConfig{
		Service: "picolet", SystemdMode: SystemdUser,
		Hostname: "vps-1",
		RepoDir:  exampleFleet,
	})
	require.NoError(t, err)

	// What Run applies and then records in the state it saves at statePath.
	st := state.NewState()
	reconciler.MergeChangeset(st, p.changeset(st))

	assert.Equal(t, "/var/lib/picolet/state.json", p.statePath())
	keys := make([]string, 0, len(st.ManagedFiles))
	for key := range st.ManagedFiles {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, []string{
		"/etc/containers/systemd/picolet/picolet.container",
		"secret:picolet_config",
	}, keys)
}
