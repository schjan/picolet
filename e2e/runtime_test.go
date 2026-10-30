//go:build e2e

package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/e2eruntime"
	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// TestMain points the SOPS corpus at the runner's read-only copy of
// testdata/: a scenario user may not be able to read the checkout.
func TestMain(m *testing.M) {
	if dir := os.Getenv("PICOLET_E2E_TESTDATA"); dir != "" {
		testsops.Dir = filepath.Join(dir, "sops")
	}
	os.Exit(m.Run())
}

// scenarioRuntime is the verified disposable runtime of a scenario with the
// standard rootless destinations of its user.
type scenarioRuntime struct {
	*e2eruntime.Runtime

	QuadletDir string
	SystemdDir string
	DataDir    string
}

// isolatedRuntime verifies that the test runs in the runtime scripts/e2e.sh
// allocated for its top-level test and fails it otherwise. Every test that
// touches real systemd or Podman calls it before anything else.
func isolatedRuntime(t *testing.T) scenarioRuntime {
	t.Helper()
	scenario, _, _ := strings.Cut(t.Name(), "/")
	rt, err := e2eruntime.Verify(e2eruntime.OS(), scenario)
	if err != nil {
		t.Fatalf("refusing to touch systemd or Podman: %v", err)
	}
	// ResolveDirs reads HOME, which Verify pinned to the allocated home.
	quadletDir, systemdDir, dataDir, err := resolver.ResolveDirs(true)
	require.NoError(t, err)
	return scenarioRuntime{Runtime: rt, QuadletDir: quadletDir, SystemdDir: systemdDir, DataDir: dataDir}
}

// requireImage makes ref available in the scenario's store: the runner loads
// the images it pulled once; a run without that archive pulls.
func requireImage(t *testing.T, ref string) {
	t.Helper()
	if exec.Command("podman", "image", "exists", ref).Run() == nil {
		return
	}
	out, err := exec.Command("podman", "pull", ref).CombinedOutput()
	require.NoError(t, err, "podman pull %s: %s", ref, out)
}

// unitProperty reads a property of unit from the scenario's user manager.
func unitProperty(t *testing.T, unit, property string) string {
	t.Helper()
	out, err := exec.Command("systemctl", "--user", "show", "-p", property, "--value", unit).Output()
	require.NoError(t, err, "systemctl --user show %s %s", property, unit)
	return strings.TrimSpace(string(out))
}

// assertGeneratedFrom checks that the user manager loaded service from
// Quadlet's generator output for the Quadlet file at source: discovery, not
// only a file on disk.
func assertGeneratedFrom(t *testing.T, rt scenarioRuntime, service, source string) {
	t.Helper()
	assert.Equal(t, "loaded", unitProperty(t, service, "LoadState"), "%s should be loaded", service)
	assert.Equal(t, source, unitProperty(t, service, "SourcePath"), "%s should be generated from %s", service, source)
	assert.Equal(t, filepath.Join(rt.RuntimeDir, "systemd", "generator", service), unitProperty(t, service, "FragmentPath"),
		"%s should come from the user manager's generator dir", service)
}

// assertStateInRuntime checks that every Managed File the state records lies
// in the scenario's home (secrets are Podman's, in the scenario's store).
func assertStateInRuntime(t *testing.T, rt scenarioRuntime, st *state.State) {
	t.Helper()
	for path := range st.ManagedFiles {
		if strings.HasPrefix(path, "secret:") {
			continue
		}
		assert.True(t, strings.HasPrefix(path, rt.Home+"/"), "managed file %s must lie in the scenario home %s", path, rt.Home)
	}
}
