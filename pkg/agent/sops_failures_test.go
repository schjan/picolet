package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/sops"
)

// The re-encrypted twin (same values, a new data key and new ciphertexts)
// renders the same files: nothing is written and nothing restarts.
func TestAgentReencryptedSopsFileChangesNothing(t *testing.T) {
	t.Parallel()
	h := newSopsHarness(t, sopsFleet(dbPasswordSecret, map[string]string{"secrets/db.sops.yaml": sopsCorpus(t, "values.sops.yaml")}),
		writeIdentityFile(t, "srv-1"))
	require.NoError(t, h.tick(t))
	require.Equal(t, "hunter2-test", h.secret("db_password"))

	pushToTestRepo(t, h.bareDir, map[string]string{"secrets/db.sops.yaml": sopsCorpus(t, "values-reencrypted.sops.yaml")})
	h.reset()
	require.NoError(t, h.tick(t))

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.secrets, "no secret rewritten")
	assert.Empty(t, h.restarts, "nothing restarted")
	syncs, _ := sopsSyncs(h.a)
	assert.Equal(t, uint64(2), syncs, "each resolve that decrypted is a sync")
}

// Decrypt failures are ordinary reconciliation failures: the SOPS package's
// message reaches the event and the deployment status, the failure gate
// counts them, and no sync is recorded.
func TestAgentSopsDecryptFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		identities []string // nil: no sops block
		file       string
		want       error
		wantText   string
	}{
		{name: "not a recipient", identities: []string{"srv-3"}, file: "values.sops.yaml", want: sops.ErrNotRecipient, wantText: "not a recipient"},
		{name: "MAC mismatch", identities: []string{"srv-1"}, file: "tampered-mac.sops.yaml", want: sops.ErrEditedOutsideSOPS, wantText: "edited outside sops"},
		{name: "no sops block", file: "values.sops.yaml", want: sops.ErrNoIdentity, wantText: "sops.age_identity_path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			identityPath := ""
			if tc.identities != nil {
				identityPath = writeIdentityFile(t, tc.identities...)
			}
			h := newSopsHarness(t, sopsFleet(dbPasswordSecret, map[string]string{"secrets/db.sops.yaml": sopsCorpus(t, tc.file)}), identityPath)

			err := h.tick(t)
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.wantText)

			events := eventsWithResult(h.a.statusStore.Snapshot(), "failure")
			require.Len(t, events, 1)
			assert.Contains(t, events[0].Message, tc.wantText)
			h.mu.Lock()
			assert.Empty(t, h.secrets)
			require.Len(t, h.failures, 1)
			assert.Contains(t, h.failures[0], tc.wantText)
			h.mu.Unlock()
			st, err := h.store.Load()
			require.NoError(t, err)
			assert.Equal(t, 1, st.FailedCount)
			syncs, _ := sopsSyncs(h.a)
			assert.Zero(t, syncs)
		})
	}
}

// A validator error may embed a rendered value: a manifest's decrypted value
// in the wrong place fails validation, and the value appears in none of the
// error, the dashboard event, the deployment status or the logs.
func TestAgentScrubsDecryptedValuesFromErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest string
	}{
		// The YAML library quotes the value truncated ("hunter2...").
		{name: "type error", manifest: "apiVersion: v1\nkind: Pod\nmetadata: {{ readSopsSecret \"sops://secrets/db.sops.yaml#/db/password\" }}\n"},
		// The kind check quotes the value whole.
		{name: "kind error", manifest: "apiVersion: v1\nkind: {{ readSopsSecret \"sops://secrets/db.sops.yaml#/db/password\" }}\nmetadata:\n  name: app\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := sopsFleet(`{{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0" }}`, map[string]string{
				"secrets/db.sops.yaml":   sopsCorpus(t, "values.sops.yaml"),
				"manifests/app.yml.tmpl": tc.manifest,
			}, "manifests/app.yml.tmpl")
			h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))

			err := h.tick(t)
			require.ErrorContains(t, err, "validation failed")
			require.ErrorContains(t, err, sops.Redacted)

			events := eventsWithResult(h.a.statusStore.Snapshot(), "failure")
			require.NotEmpty(t, events)
			h.mu.Lock()
			require.Len(t, h.failures, 1)
			texts := map[string]string{"error": err.Error(), "deployment status": h.failures[0], "logs": h.logs.String()}
			h.mu.Unlock()
			for i, e := range events {
				texts["event "+strconv.Itoa(i)] = e.Message
			}
			for where, text := range texts {
				for _, leak := range []string{"hunter2-test", "hunter2...", "alpha-token"} {
					assert.NotContains(t, text, leak, "%s leaks a decrypted value", where)
				}
			}
		})
	}
}

// A scrubbed error still answers errors.Is for its chain, but hides the
// unscrubbed text from errors.Unwrap and errors.As.
func TestScrubErrorKeepsErrorsIs(t *testing.T) {
	t.Parallel()
	require.NoError(t, scrubError(nil, nil))
	plain := errors.New("plain")
	assert.Same(t, plain, scrubError(nil, plain))

	reader := sops.NewReader(os.DirFS(sopsCorpusDir), mustIdentities(t, "srv-1"))
	_, err := reader.ReadAll(t.Context(), []string{"sops://corpus/values.sops.yaml#/db/password"})
	require.NoError(t, err)
	assert.Same(t, plain, scrubError(reader, plain), "nothing to scrub: the error as is")

	leaky := &applier.HookFallbackError{Unit: "app.service", Err: errors.New("hunter2-test")}
	wrapped := errors.Join(applier.ErrApplyIncomplete, leaky)
	scrubbed := scrubError(reader, wrapped)
	assert.NotContains(t, scrubbed.Error(), "hunter2-test")
	require.ErrorIs(t, scrubbed, applier.ErrApplyIncomplete)
	require.NoError(t, errors.Unwrap(scrubbed))
	_, ok := errors.AsType[*applier.HookFallbackError](scrubbed)
	assert.False(t, ok)
}

func mustIdentities(t *testing.T, names ...string) *sops.Identities {
	t.Helper()
	data, err := os.ReadFile(writeIdentityFile(t, names...))
	require.NoError(t, err)
	ids, err := sops.ParseIdentities(data)
	require.NoError(t, err)
	return ids
}

// A broken identity file stops the Agent at startup.
func TestAgentStartupRefusesBrokenIdentityFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	garbage := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(garbage, []byte("not an identity\n"), 0o600))
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("# rotated out\n"), 0o600))

	for name, path := range map[string]string{
		"missing": filepath.Join(dir, "missing"), "unreadable": unreadable, "garbage": garbage, "no identity": empty,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentcfg.Config{
				Hostname: "test-host", RepoURL: "https://example.com/fleet.git", ListenAddr: "127.0.0.1:0",
				Sops: &agentcfg.SopsConfig{AgeIdentityPath: path},
			}
			a := newTestAgent(t, cfg, WithRepoPath(filepath.Join(t.TempDir(), "clone")),
				WithStatePath(filepath.Join(t.TempDir(), "state.json")))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := a.Run(ctx)
			require.ErrorContains(t, err, "setting up sops")
			require.ErrorContains(t, err, path)
		})
	}
}

func TestAgentWarnsOnGroupReadableIdentityFile(t *testing.T) {
	t.Parallel()
	path := writeIdentityFile(t, "srv-1")
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o604, true}} {
		require.NoError(t, os.Chmod(path, tc.mode))
		logger, logs := testlog.New()
		a := newTestAgent(t, &agentcfg.Config{Hostname: "test-host", Sops: &agentcfg.SopsConfig{AgeIdentityPath: path}}, WithLogger(logger))
		require.NoError(t, a.setupSops())
		assert.Equal(t, tc.warn, logs.Has(slog.LevelWarn, "age identity file is readable by group or others; restrict it to the Agent (chmod 600)"), "mode %o", tc.mode)
		assert.Equal(t, 1, a.sopsIDs.Len())
	}
}
