package agent

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/sops"
	"github.com/schjan/picolet/pkg/state"
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
			deployment := h.failures[0]
			h.mu.Unlock()
			assertNoFixturePlaintext(t, map[string]string{
				"error": err.Error(), "dashboard event": events[0].Message, "deployment status": deployment,
			})
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

// A tick whose diff is empty still validates; a validation failure there
// (a validator upgrade, a secret-refresh tick) reaches the dashboard event
// and the logs scrubbed, like one on the changes path.
func TestAgentScrubsDecryptedValuesFromNoChangesValidation(t *testing.T) {
	t.Parallel()
	files := sopsFleet(`{{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0" }}`, map[string]string{
		"secrets/db.sops.yaml":   sopsCorpus(t, "values.sops.yaml"),
		"manifests/app.yml.tmpl": "apiVersion: v1\nkind: {{ readSopsSecret \"sops://secrets/db.sops.yaml#/db/password\" }}\nmetadata:\n  name: app\n",
	}, "manifests/app.yml.tmpl")
	h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))
	require.Error(t, h.tick(t), "the first tick clones the Fleet and fails validation")

	// Record the rendered files as already managed, so the diff is empty.
	resolved, _, err := h.a.loadAndResolve(t.Context())
	require.NoError(t, err)
	st, err := h.store.Load()
	require.NoError(t, err)
	UpdateState(st, reconciler.Diff(resolved.Files, st))
	require.False(t, reconciler.Diff(resolved.Files, st).HasChanges())
	before := len(eventsWithResult(h.a.statusStore.Snapshot(), "failure"))

	_, err = h.a.ReconcileOnce(t.Context(), "no-changes-sha", st, h.store)
	require.ErrorContains(t, err, "validation failed")
	require.ErrorContains(t, err, sops.Redacted)

	events := eventsWithResult(h.a.statusStore.Snapshot(), "failure")
	require.Len(t, events, before+1)
	texts := map[string]string{"error": err.Error(), "event": events[len(events)-1].Message, "logs": h.logs.String()}
	for where, text := range texts {
		for _, leak := range []string{"hunter2-test", "alpha-token"} {
			assert.NotContains(t, text, leak, "%s leaks a decrypted value", where)
		}
	}
}

// The apply's own log lines (the rollback notice, non-fatal apply errors)
// carry the error text, so they are scrubbed with the resolve's reader too.
func TestApplyScrubsLoggedErrors(t *testing.T) {
	t.Parallel()
	leak := errors.New("backend said: hunter2-test")
	secretChange := &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}
	tests := map[string]func(t *testing.T, a *Agent, sys *appliermocks.MockSystemdManager, pod *appliermocks.MockPodmanClient, reader *sops.Reader){
		"apply failed, rolling back": func(t *testing.T, a *Agent, sys *appliermocks.MockSystemdManager, pod *appliermocks.MockPodmanClient, reader *sops.Reader) {
			t.Helper()
			pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(leak)
			sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
			_, err := a.applyWithRollback(t.Context(), "sha", secretChange, nil, nil, nil, "", "", reader)
			require.ErrorIs(t, err, errRollbackPerformed)
		},
		"non-fatal apply error": func(t *testing.T, a *Agent, _ *appliermocks.MockSystemdManager, _ *appliermocks.MockPodmanClient, reader *sops.Reader) {
			t.Helper()
			st := state.NewState()
			store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
			_, err := a.finalizeApply("sha", st, store, secretChange, &applier.ApplyResult{Errors: []error{leak}}, nil, refSecretsCounts{}, nil, reader)
			require.NoError(t, err)
		},
	}
	for message, run := range tests {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			reader := sops.NewReader(os.DirFS(sopsCorpusDir), mustIdentities(t, "srv-1"))
			_, err := reader.ReadAll(t.Context(), []string{"sops://corpus/values.sops.yaml#/db/password"})
			require.NoError(t, err)
			sys, pod, fw := newBareMocks(t)
			logger, logs := testlog.New()
			a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw), WithLogger(logger))

			run(t, a, sys, pod, reader)

			assert.Contains(t, logs.String(), message)
			assert.Contains(t, logs.String(), sops.Redacted)
			assert.NotContains(t, logs.String(), "hunter2-test")
		})
	}
}

// A SOPS sync is a resolve that decrypted at least one file, so it is
// counted even when the tick then fails (here: validation).
func TestAgentCountsSopsSyncOnFailedTick(t *testing.T) {
	t.Parallel()
	files := sopsFleet(dbPasswordSecret, map[string]string{
		"secrets/db.sops.yaml":   sopsCorpus(t, "values.sops.yaml"),
		"manifests/app.yml.tmpl": "apiVersion: v1\nkind: Pod\nmetadata: [unclosed\n",
	}, "manifests/app.yml.tmpl")
	h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))

	require.ErrorContains(t, h.tick(t), "validation failed")
	syncs, synced := sopsSyncs(h.a)
	assert.Equal(t, uint64(1), syncs)
	assert.True(t, synced)
}

// fixturePlaintexts are the values of testdata/sops/plain/values.yaml long
// enough to be recognisable in a message.
var fixturePlaintexts = []string{
	"hunter2-test", "alpha-token", "beta-token", "gamma-secret", "srv1-pass",
	"slash-key", "tilde-key", "5432", "0.75", "visible",
}

// hashLike matches a hex run as long as a truncated hash (SOPS's MAC is a
// 128-digit SHA-512).
var hashLike = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)

// assertNoFixturePlaintext fails when a text (keyed by where it appeared)
// carries a fixture plaintext, its SHA-256 or SHA-512, or any hash-like run.
func assertNoFixturePlaintext(t *testing.T, texts map[string]string) {
	t.Helper()
	for where, text := range texts {
		for _, p := range fixturePlaintexts {
			sum256, sum512 := sha256.Sum256([]byte(p)), sha512.Sum512([]byte(p))
			for _, leak := range []string{p, hex.EncodeToString(sum256[:]), hex.EncodeToString(sum512[:])} {
				assert.NotContains(t, strings.ToLower(text), strings.ToLower(leak), "%s leaks a fixture plaintext or its hash", where)
			}
		}
		assert.Empty(t, hashLike.FindAllString(text, -1), "%s carries a hash-like hex run", where)
	}
}
