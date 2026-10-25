package bootstrap

import (
	"maps"
	"os"
	"path/filepath"
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
	require.ErrorContains(t, err, "host node-1: agent bundle not assigned: picolet-system")
}

// bootstrap's strict render fails on readSopsSecret and readSopsData in the
// Agent bundle and says why: a credential that fetches the Fleet can't be a
// SOPS value in it.
func TestResolveAgentRejectsSopsSecret(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"readSopsSecret", "readSopsData"} {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			resolveAgentRejectsSops(t, fn)
		})
	}
}

func resolveAgentRejectsSops(t *testing.T, fn string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"fleet.yml":        "images: {picolet: \"picolet:test\"}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":  "base: {}\nroles:\n  node:\n    services: [picolet-system]\nfeatures: {}\n",
		"hosts/h/host.yml": "hostname: h\nrole: node\n",
		"services/picolet-system/picolet-system.container.tmpl": "[Container]\nImage={{ index .Images \"picolet\" }}\n" +
			"Secret=picolet_system_config,target=/etc/picolet/config.yml\n",
		"services/picolet-system/secrets/picolet_system_config.yml.tmpl": "hostname: h\n" +
			"git_token: {{ " + fn + " \"sops://secrets/git.sops.yaml#/token\" }}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	_, err := ResolveAgent(t.Context(), dir, "h")
	require.ErrorContains(t, err, fn+` "sops://secrets/git.sops.yaml#/token"`)
	require.ErrorContains(t, err, "deliver the value as a --secrets-dir file and point the Agent config at its path; "+
		"credentials needed to fetch the Fleet can't be SOPS values")
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

// writeAgentFleet writes a one-Host Fleet (h, rootful Agent bundle
// picolet-system) to a temp dir, plus extra files, and returns its dir.
func writeAgentFleet(t *testing.T, agentConfig string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"fleet.yml":        "images: {picolet: \"picolet:test\"}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":  "base: {}\nroles:\n  node:\n    services: [picolet-system]\nfeatures: {}\n",
		"hosts/h/host.yml": "hostname: h\nrole: node\n",
		"services/picolet-system/picolet-system.container.tmpl": "[Container]\nImage={{ index .Images \"picolet\" }}\n" +
			"Secret=picolet_system_config,target=/etc/picolet/config.yml\n",
		"services/picolet-system/secrets/picolet_system_config.yml.tmpl": agentConfig,
	}
	maps.Copy(files, extra)
	for name, content := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return dir
}

// bootstrap refuses to deploy a credential the Agent config it renders
// names, as the Agent does: the Agent sees its secrets dir at secrets_dir,
// the bootstrap at --secrets-dir, so a credential below secrets_dir is
// refused at the same place below --secrets-dir. Other files there deploy.
func TestBootstrapRefusesAgentCredential(t *testing.T) {
	t.Parallel()
	const agentConfig = "hostname: h\nrepo_url: https://example.com/fleet.git\n" +
		"sops:\n  age_identity_path: /etc/picolet/secrets/age-identity\n"
	secretsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "age-identity"), []byte("AGE-SECRET-KEY-FIXTURE"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "api-key"), []byte("api"), 0o600))
	run := func(secret string) error {
		dir := writeAgentFleet(t, agentConfig, map[string]string{
			"services/picolet-system/secrets/leak.tmpl": "{{ readSecretFile \"" + secret + "\" }}",
		})
		_, err := prepare(t.Context(), RunConfig{
			Service: "picolet-system", SystemdMode: SystemdSystem,
			Hostname: "h", RepoDir: dir, SecretsDir: secretsDir,
		})
		return err
	}

	err := run("age-identity")
	require.ErrorContains(t, err, "Agent credential sops.age_identity_path")
	require.ErrorContains(t, err, "Agent credentials cannot be deployed")
	assert.NotContains(t, err.Error(), "AGE-SECRET-KEY-FIXTURE")

	require.NoError(t, run("api-key"))
}

// Without a Fleet-rendered Agent config (the unit mounts none; the health
// address comes from --health-addr), bootstrap cannot know the Agent's
// credentials, yet it still refuses the conventional identity file
// age-identity below --secrets-dir.
func TestBootstrapRefusesConventionalIdentityWithoutRenderedConfig(t *testing.T) {
	t.Parallel()
	secretsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "age-identity"), []byte("AGE-SECRET-KEY-FIXTURE"), 0o600))
	dir := writeAgentFleet(t, "unused: true\n", map[string]string{ //nolint:gosec // G101: Fleet file names and a template, not credentials
		"services/picolet-system/picolet-system.container.tmpl": "[Container]\nImage={{ index .Images \"picolet\" }}\n",
		"services/picolet-system/secrets/leak.tmpl":             `{{ readSecretFile "age-identity" }}`,
	})
	_, err := prepare(t.Context(), RunConfig{
		Service: "picolet-system", SystemdMode: SystemdSystem,
		Hostname: "h", RepoDir: dir, SecretsDir: secretsDir, HealthAddr: "127.0.0.1:9418",
	})
	require.ErrorContains(t, err, "Agent credentials cannot be deployed")
	assert.NotContains(t, err.Error(), "AGE-SECRET-KEY-FIXTURE")
}

// The guard comes from the Agent config rendered with placeholder secrets;
// a config whose credential settings change once the real secret files are
// read would deploy credentials the guard never saw, so bootstrap fails.
func TestBootstrapRefusesCredentialsThatDependOnSecretFiles(t *testing.T) {
	t.Parallel()
	const agentConfig = "hostname: h\nrepo_url: https://example.com/fleet.git\n" +
		"git_token_path: {{ readSecretFile \"token-path\" }}\n"
	secretsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "token-path"), []byte("/etc/picolet/secrets/git-token"), 0o600))
	_, err := prepare(t.Context(), RunConfig{
		Service: "picolet-system", SystemdMode: SystemdSystem,
		Hostname: "h", RepoDir: writeAgentFleet(t, agentConfig, nil), SecretsDir: secretsDir,
	})
	require.ErrorContains(t, err, "credential settings must not depend on a secret file")
}
