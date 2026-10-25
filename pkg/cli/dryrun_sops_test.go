package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/state"
)

// writeFiles writes files (path relative to root → content) below root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

func readCorpus(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sops", rel))
	require.NoError(t, err)
	return string(data)
}

// `dry-run --config` decrypts like the Agent: once the state records a
// SOPS-backed file, the dry-run reports no change for it, even after the
// file was re-encrypted with a new data key.
func TestDryRunWithConfigDecryptsSops(t *testing.T) {
	t.Parallel()
	fleet := t.TempDir()
	writeFiles(t, fleet, map[string]string{ //nolint:gosec // G101: test templates, no credentials
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          "base:\n  secrets:\n    - secrets/db_password.tmpl\n",
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\n",
		"secrets/db_password.tmpl": `{{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		"secrets/db.sops.yaml":     readCorpus(t, "corpus/values.sops.yaml"),
	})
	dir := t.TempDir()
	identity := filepath.Join(dir, "age-identity")
	require.NoError(t, os.WriteFile(identity, []byte(readCorpus(t, "identities/srv-1.agekey")), 0o600))
	dataDir := filepath.Join(dir, "data")
	configPath := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(configPath, []byte("hostname: test-host\nsecrets_dir: "+filepath.Join(dir, "secrets")+
		"\ndata_dir: "+dataDir+"\nsops:\n  age_identity_path: "+identity+"\n"), 0o600))

	changeset, err := dryRunChangeset(t.Context(), fleet, "test-host", configPath)
	require.NoError(t, err)
	require.True(t, changeset.HasChanges())
	assert.Equal(t, 1, changeset.Summary[reconciler.ActionCreate])

	// Record the applied state, as an apply would.
	st := state.NewState()
	agent.UpdateState(st, changeset)
	require.NoError(t, state.NewStore(filepath.Join(dataDir, "state.json")).Save(st))

	changeset, err = dryRunChangeset(t.Context(), fleet, "test-host", configPath)
	require.NoError(t, err)
	assert.False(t, changeset.HasChanges(), "an unchanged SOPS-backed file is not a change")

	writeFiles(t, fleet, map[string]string{"secrets/db.sops.yaml": readCorpus(t, "corpus/values-reencrypted.sops.yaml")})
	changeset, err = dryRunChangeset(t.Context(), fleet, "test-host", configPath)
	require.NoError(t, err)
	assert.False(t, changeset.HasChanges(), "a re-encrypted file with the same values is not a change")
}
