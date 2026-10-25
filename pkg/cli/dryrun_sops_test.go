package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/sops"
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

// corpusRecipientOf returns the TEST-ONLY corpus recipient called name.
func corpusRecipientOf(t *testing.T, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(readCorpus(t, "recipients.txt"), "\n") {
		if n, key, ok := strings.Cut(line, " "); ok && n == name {
			return key
		}
	}
	t.Fatalf("recipient %s not in the corpus", name)
	return ""
}

func readCorpus(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sops", rel))
	require.NoError(t, err)
	return string(data)
}

// sopsConfigFleet writes a Fleet whose Host renders a readSopsSecret value
// (plus extra files) and an Agent config that names the srv-1 identity. It
// returns the Fleet dir, the config path and the data dir.
func sopsConfigFleet(t *testing.T, extra map[string]string) (fleet, configPath, dataDir string) {
	t.Helper()
	fleet = t.TempDir()
	files := map[string]string{ //nolint:gosec // G101: test templates, no credentials
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          "base:\n  secrets:\n    - secrets/db_password.tmpl\n",
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\nage_recipients: [" + corpusRecipientOf(t, "srv-1") + "]\n",
		"secrets/db_password.tmpl": `{{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		"secrets/db.sops.yaml":     readCorpus(t, "corpus/values.sops.yaml"),
	}
	maps.Copy(files, extra)
	writeFiles(t, fleet, files)
	dir := t.TempDir()
	identity := filepath.Join(dir, "age-identity")
	require.NoError(t, os.WriteFile(identity, []byte(readCorpus(t, "identities/srv-1.agekey")), 0o600))
	dataDir = filepath.Join(dir, "data")
	configPath = filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(configPath, []byte("hostname: test-host\nsecrets_dir: "+filepath.Join(dir, "secrets")+
		"\ndata_dir: "+dataDir+"\nsops:\n  age_identity_path: "+identity+"\n"), 0o600))
	return fleet, configPath, dataDir
}

// `dry-run --config` decrypts like the Agent: once the state records a
// SOPS-backed file, the dry-run reports no change for it, even after the
// file was re-encrypted with a new data key.
func TestDryRunWithConfigDecryptsSops(t *testing.T) {
	t.Parallel()
	fleet, configPath, dataDir := sopsConfigFleet(t, nil)

	changeset := dryRunChanges(t, fleet, configPath, "")
	require.True(t, changeset.HasChanges())
	assert.Equal(t, 1, changeset.Summary[reconciler.ActionCreate])

	// Record the applied state, as an apply would.
	st := state.NewState()
	agent.UpdateState(st, changeset)
	require.NoError(t, state.NewStore(filepath.Join(dataDir, "state.json")).Save(st))

	changeset = dryRunChanges(t, fleet, configPath, "")
	assert.False(t, changeset.HasChanges(), "an unchanged SOPS-backed file is not a change")

	writeFiles(t, fleet, map[string]string{"secrets/db.sops.yaml": readCorpus(t, "corpus/values-reencrypted.sops.yaml")})
	changeset = dryRunChanges(t, fleet, configPath, "")
	assert.False(t, changeset.HasChanges(), "a re-encrypted file with the same values is not a change")
}

// dryRunChanges runs a dry-run's resolve, validation and diff (with the
// Agent config at configPath, or keyless against the state at statePath
// when configPath is "") and returns its changeset.
func dryRunChanges(t *testing.T, fleet, configPath, statePath string) *reconciler.Changeset {
	t.Helper()
	result, err := dryRunChangeset(t.Context(), fleet, "test-host", configPath, statePath)
	require.NoError(t, err)
	return result.Changeset
}

// Without an Agent config a dry-run resolves keyless, like resolve: a
// sops:// value renders as the placeholder, so a file whose render used one
// can't be compared with what the Agent applied. Such a file shows as
// changed but unverified; every other file diffs as usual.
func TestDryRunWithoutConfigIsKeyless(t *testing.T) {
	t.Parallel()
	fleet, configPath, dataDir := sopsConfigFleet(t, map[string]string{
		"assignments.yml": "base:\n  secrets:\n    - secrets/db_password.tmpl\n    - secrets/plain.tmpl\n" +
			"    - \"sops://secrets/db.sops.yaml#/db/user\"\n",
		"secrets/plain.tmpl": "plain",
	})
	statePath := filepath.Join(dataDir, "state.json")

	result, err := dryRunChangeset(t.Context(), fleet, "test-host", "", statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, result.Changeset.Summary[reconciler.ActionCreate])

	// The Agent applied the decrypted values.
	st := state.NewState()
	agent.UpdateState(st, dryRunChanges(t, fleet, configPath, ""))
	require.NoError(t, state.NewStore(statePath).Save(st))
	writeFiles(t, fleet, map[string]string{"secrets/plain.tmpl": "plain, edited"})

	result, err = dryRunChangeset(t.Context(), fleet, "test-host", "", statePath)
	require.NoError(t, err)
	var logs bytes.Buffer
	logDryRun(slog.New(slog.NewJSONHandler(&logs, nil)), result)
	actions := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var rec struct{ Msg, Action, Path string }
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec.Msg == "change" {
			actions[rec.Path] = rec.Action
		}
	}
	assert.Equal(t, map[string]string{
		"secret:db_password": "update (unverified: SOPS values not decrypted)",
		"secret:db_user":     "update (unverified: SOPS values not decrypted)",
		"secret:plain":       "update",
	}, actions)
}

// `apply --config` does not decrypt (spec #248: considered, not adopted),
// even with a sops block: a sops:// ref fails with the no-identity error and
// a message that says apply never decrypts, rather than only pointing at the
// sops block the config already has.
func TestApplyWithConfigDoesNotDecryptSops(t *testing.T) {
	t.Parallel()
	fleet, configPath, _ := sopsConfigFleet(t, nil)

	err := runApply(t.Context(), configPath, fleet, "test-host")
	require.ErrorIs(t, err, sops.ErrNoIdentity)
	require.ErrorContains(t, err, "picolet apply does not decrypt sops:// refs")
	require.ErrorContains(t, err, "sops://", "the message still names the failing ref")
	require.NotContains(t, err.Error(), "set sops.age_identity_path",
		"apply must not point at a sops block that would not help it")
}

// `dry-run --config` decrypts, so a validation error that embeds a decrypted
// value (a manifest's YAML type error) reaches the terminal scrubbed.
func TestDryRunWithConfigScrubsValidationErrors(t *testing.T) {
	t.Parallel()
	fleet, configPath, _ := sopsConfigFleet(t, map[string]string{
		"assignments.yml":        "base:\n  paths:\n    - manifests/app.yml.tmpl\n  secrets:\n    - secrets/db_password.tmpl\n",
		"manifests/app.yml.tmpl": "apiVersion: v1\nkind: Pod\nmetadata: {{ readSopsSecret \"sops://secrets/db.sops.yaml#/db/password\" }}\n",
	})

	_, err := dryRunChangeset(t.Context(), fleet, "test-host", configPath, "")
	require.ErrorContains(t, err, "validation failed")
	require.ErrorContains(t, err, sops.Redacted)
	for _, leak := range []string{"hunter2-test", "hunter2..."} {
		assert.NotContains(t, err.Error(), leak)
	}
}
