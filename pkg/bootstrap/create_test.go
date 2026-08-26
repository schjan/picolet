package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateRootfulScript(t *testing.T) {
	t.Parallel()
	dir := writeCreateFleet(t, "picolet-system")
	var out bytes.Buffer

	err := Create(t.Context(), CreateConfig{
		Hostname:      "node-1",
		FleetDir:      dir,
		TargetPath:    "/tmp/fleet",
		SkipGitChecks: true,
		Stdout:        &out,
		Stderr:        &bytes.Buffer{},
	})
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "# Service: picolet-system")
	assert.Contains(t, got, "--security-opt apparmor=unconfined")
	assert.Contains(t, got, "--service=picolet-system")
	assert.Contains(t, got, "/etc/picolet/secrets/git_token")
	// The watch hint must be a comment: the plan is also the basis for shell
	// execution, and an uncommented `journalctl -fu` never terminates.
	assert.Contains(t, got, "#   sudo journalctl -fu picolet-system.service")
}

// The remote variant is piped to `ssh ... bash -s`; it must contain nothing
// that blocks (journalctl -fu) or re-runs the local rsync on the remote host.
func TestRenderRemoteScript(t *testing.T) {
	t.Parallel()
	got, err := renderCreateOutput("remote", createScriptData{
		Hostname:   "node-1",
		FleetDir:   "/tmp/fleet-src",
		TargetPath: "/tmp/fleet",
		Service:    "picolet-system",
		Image:      "ghcr.io/schjan/picolet:test",
	})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(got, "set -euo pipefail\n"))
	assert.Contains(t, got, "sudo podman run --rm")
	assert.NotContains(t, got, "journalctl")
	assert.NotContains(t, got, "rsync")
}

func TestCreateRootlessScript(t *testing.T) {
	t.Parallel()
	dir := writeCreateFleet(t, "picolet")
	var out bytes.Buffer

	err := Create(t.Context(), CreateConfig{
		Hostname:      "node-1",
		FleetDir:      dir,
		TargetPath:    "/tmp/fleet",
		SkipGitChecks: true,
		Stdout:        &out,
		Stderr:        &bytes.Buffer{},
	})
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "# Service: picolet")
	assert.Contains(t, got, "-v $HOME/.config/picolet:/etc/picolet")
	assert.Contains(t, got, "--service=picolet")
	assert.Contains(t, got, "--systemd=user")
}

func writeCreateFleet(t *testing.T, service string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "fleet.yml", `images:
  picolet: "ghcr.io/schjan/picolet:test"
ports:
  picolet_metrics: 9417
  picolet_system_metrics: 9418
`)
	writeFile(t, dir, "assignments.yml", "base: {}\nroles:\n  node:\n    services: ["+service+"]\nfeatures: {}\n")
	writeFile(t, dir, "hosts/node-1/host.yml", "hostname: node-1\nrole: node\n")
	metricsKey := "picolet_metrics"
	configName := "picolet_config"
	if service == "picolet-system" {
		metricsKey = "picolet_system_metrics"
		configName = "picolet_system_config"
	}
	writeFile(t, dir, "services/"+service+"/containers/"+service+".container.tmpl",
		"[Container]\nImage={{ index .Images \"picolet\" }}\nSecret="+configName+",target=/etc/picolet/config.yml\n")
	writeFile(t, dir, "services/"+service+"/secrets/"+configName+".yml.tmpl", `hostname: "{{ .Host.Hostname }}"
repo_url: "https://example.test/fleet.git"
git_token_path: "/etc/picolet/secrets/git_token"
metrics_port: {{ index .Ports "`+metricsKey+`" }}
`)
	return dir
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
