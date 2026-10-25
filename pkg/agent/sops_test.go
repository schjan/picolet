package agent

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	agentmocks "github.com/schjan/picolet/mocks/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// dbPasswordSecret is a secret template reading the corpus' db password.
const dbPasswordSecret = `{{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}` //nolint:gosec // G101: a template, not a credential

// sopsCorpusDir is the TEST-ONLY SOPS fixture corpus.
var sopsCorpusDir = filepath.Join("..", "..", "testdata", "sops")

func sopsCorpus(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sopsCorpusDir, "corpus", name))
	require.NoError(t, err)
	return string(data)
}

// writeIdentityFile writes the TEST-ONLY corpus identities called names into
// one identity file (mode 0600) and returns its path.
func writeIdentityFile(t *testing.T, names ...string) string {
	t.Helper()
	var data []byte
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(sopsCorpusDir, "identities", name+".agekey"))
		require.NoError(t, err)
		data = append(data, b...)
	}
	path := filepath.Join(t.TempDir(), "age-identity")
	require.NoError(t, os.WriteFile(path, data, 0o600)) //nolint:gosec // G703: a path below t.TempDir()
	return path
}

// sopsFleet returns a Fleet for test-host: the Podman secret db_password is
// rendered from secret (a template body) and consumed by app.container;
// extra adds Fleet files (SOPS files, manifests) and assignment paths.
func sopsFleet(secret string, extra map[string]string, paths ...string) map[string]string {
	assignments := "base:\n  paths:\n    - quadlets/app.container\n"
	for _, p := range paths {
		assignments += "    - " + p + "\n"
	}
	assignments += "  secrets:\n    - secrets/db_password.tmpl\n"
	files := map[string]string{
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          assignments,
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\n",
		"quadlets/app.container":   "[Container]\nImage=docker.io/library/alpine:3\nSecret=db_password\n",
		"secrets/db_password.tmpl": secret,
	}
	maps.Copy(files, extra)
	return files
}

// initFleetRepo commits files to a fresh repo and returns a bare clone of
// it to use as RepoURL.
func initFleetRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	workDir := filepath.Join(t.TempDir(), "work")
	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)
	for path, content := range files {
		writeTestFile(t, workDir, path, content)
	}
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@test.com"}})
	require.NoError(t, err)
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return bareDir
}

// sopsHarness drives the Agent loop (tick by tick) over an in-memory git
// Fleet and records what reaches Podman, systemd, the deployment reporter and
// the logs.
type sopsHarness struct {
	a       *Agent
	bareDir string
	poller  *gitpoll.Poller
	store   *state.Store
	health  *health.Checker
	logs    *testlog.Recorder

	mu        sync.Mutex
	secrets   map[string]string // Podman secrets created, by name
	restarts  []string
	failures  []string // texts of failed deployment statuses
	successes int
}

// newSopsHarness builds an Agent for test-host over files; identityPath is
// the Agent config's sops.age_identity_path ("" for no sops block). The
// Agent's startup loads the identities.
func newSopsHarness(t *testing.T, files map[string]string, identityPath string) *sopsHarness {
	t.Helper()
	h := &sopsHarness{bareDir: initFleetRepo(t, files), secrets: map[string]string{}}
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.Anything).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, unit string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.restarts = append(h.restarts, unit)
		return nil
	}).Maybe()
	pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name string, data []byte, _ bool) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.secrets[name] = string(data)
			return nil
		}).Maybe()
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: h.bareDir, RepoBranch: "master", SecretsDir: t.TempDir()}
	if identityPath != "" {
		cfg.Sops = &agentcfg.SopsConfig{AgeIdentityPath: identityPath}
	}
	logger, logs := testlog.New()
	h.logs = logs
	h.a = newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath), WithDataDir(t.TempDir()),
		WithDeploymentReporter(h.reporter(t)), WithLogger(logger))
	require.NoError(t, h.a.setupSops())

	h.store = state.NewStore(statePath)
	h.poller = gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil, gitpoll.WithLogger(discardLogger()))
	require.NoError(t, h.poller.Init(t.Context()))
	h.health = health.New(sys, health.WithLogger(discardLogger()))
	return h
}

// reporter returns a deployment reporter that records the statuses.
func (h *sopsHarness) reporter(t *testing.T) *agentmocks.MockDeploymentReporter {
	t.Helper()
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().CreateDeployment(mock.Anything, mock.Anything).Return(int64(1), nil).Maybe()
	reporter.EXPECT().ReportInProgress(mock.Anything, mock.Anything).Return(nil).Maybe()
	reporter.EXPECT().ReportSuccess(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, int64) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.successes++
		return nil
	}).Maybe()
	recordFailure := func(_ context.Context, _ int64, err error) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.failures = append(h.failures, err.Error())
		return nil
	}
	reporter.EXPECT().ReportFailure(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(recordFailure).Maybe()
	reporter.EXPECT().ReportError(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(recordFailure).Maybe()
	return reporter
}

func (h *sopsHarness) tick(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return h.a.tick(ctx, h.poller, h.store, h.health)
}

// reset forgets what the previous ticks did.
func (h *sopsHarness) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets = map[string]string{}
	h.restarts = nil
}

func (h *sopsHarness) secret(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.secrets[name]
}

// sopsSyncs returns the Agent's provider="sops" sync count and whether a
// last-sync time is recorded.
func sopsSyncs(a *Agent) (uint64, bool) {
	p := a.statusStore.SecretProviders()[string(resolver.ProviderSops)]
	return p.Syncs, !p.LastSyncAt.IsZero()
}

func TestAgentDeploysDecryptedSopsValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		identities []string
		files      map[string]string
		secret     string
		want       string
	}{
		{
			name:       "one identity",
			identities: []string{"srv-1"},
			files:      map[string]string{"secrets/db.sops.yaml": "values.sops.yaml"}, //nolint:gosec // G101: file names, no credentials
			secret:     dbPasswordSecret,
			want:       "hunter2-test",
		},
		{
			// Rotation: the identity file holds two keys, and each file is
			// encrypted to only one of them (values.sops.json to srv-2,
			// pq.sops.yaml to the hybrid post-quantum srv-pq).
			name:       "rotation",
			identities: []string{"srv-2", "srv-pq"},
			files: map[string]string{ //nolint:gosec // G101: file names, no credentials
				"secrets/old.sops.json": "values.sops.json",
				"secrets/new.sops.yaml": "pq.sops.yaml",
			},
			secret: `{{ readSopsSecret "sops://secrets/old.sops.json#/tokens/0" }}/` +
				`{{ readSopsSecret "sops://secrets/new.sops.yaml#/db/password" }}`,
			want: "alpha-token/hunter2-test",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			extra := map[string]string{}
			for dst, src := range tc.files {
				extra[dst] = sopsCorpus(t, src)
			}
			h := newSopsHarness(t, sopsFleet(tc.secret, extra), writeIdentityFile(t, tc.identities...))
			require.NoError(t, h.tick(t))

			assert.Equal(t, tc.want, h.secret("db_password"))
			assert.Equal(t, []string{"app.service"}, h.restarts)
			// A sync is one resolve that decrypted at least one SOPS file;
			// this Host has no direct secrets.
			require.NoError(t, testutil.CollectAndCompare(metrics.NewSecretProviderCollector(h.a.statusStore), strings.NewReader(`
# HELP picolet_secret_sync_total Successful secret-provider sync attempts, per provider. Failures are counted on picolet_reconciliation_total{result="failure"}.
# TYPE picolet_secret_sync_total counter
picolet_secret_sync_total{provider="sops"} 1
`), "picolet_secret_sync_total"))
			_, synced := sopsSyncs(h.a)
			assert.True(t, synced, "last-sync time recorded")
			assert.True(t, h.logs.Has(slog.LevelInfo, "sops identities loaded"))
		})
	}
}

// A direct sops:// secrets: entry creates the Podman secret named after its
// pointer (db_password) with the decrypted value, and counts toward
// picolet_secrets_managed_count{provider="sops"}. The corpus' re-encrypted
// twin (same values, new data key and ciphertexts) changes no secret and
// restarts nothing.
func TestAgentDeploysDirectSopsSecret(t *testing.T) {
	t.Parallel()
	files := sopsFleet("", map[string]string{"secrets/db.sops.yaml": sopsCorpus(t, "values.sops.yaml")})
	delete(files, "secrets/db_password.tmpl")
	files["assignments.yml"] = "base:\n  paths:\n    - quadlets/app.container\n" +
		"  secrets:\n    - \"sops://secrets/db.sops.yaml#/db/password\"\n"
	h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))
	require.NoError(t, h.tick(t))

	assert.Equal(t, "hunter2-test", h.secret("db_password"))
	assert.Equal(t, []string{"app.service"}, h.restarts)
	require.NoError(t, testutil.CollectAndCompare(metrics.NewSecretProviderCollector(h.a.statusStore), strings.NewReader(`
# HELP picolet_secrets_managed_count Number of direct provider-backed secret assignments currently managed, per provider.
# TYPE picolet_secrets_managed_count gauge
picolet_secrets_managed_count{provider="sops"} 1
`), "picolet_secrets_managed_count"))

	pushToTestRepo(t, h.bareDir, map[string]string{"secrets/db.sops.yaml": sopsCorpus(t, "values-reencrypted.sops.yaml")})
	h.reset()
	require.NoError(t, h.tick(t))
	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.secrets, "no secret rewritten")
	assert.Empty(t, h.restarts, "nothing restarted")
}

// A Managed File whose render used a SOPS value is recorded as such in the
// state the dashboard reads, so the dashboard can hide its content hash (a
// hash of the decrypted value); a file without SOPS values is not.
func TestAgentRecordsSopsValuesInState(t *testing.T) {
	t.Parallel()
	files := sopsFleet(dbPasswordSecret, map[string]string{
		"secrets/db.sops.yaml": sopsCorpus(t, "values.sops.yaml"),
		"files/pin.tmpl":       `pin={{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		"files/plain.txt":      "plain",
	}, "files/pin.tmpl", "files/plain.txt")
	h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))
	require.NoError(t, h.tick(t))

	st, err := h.store.Load()
	require.NoError(t, err)
	sopsValues := map[string]bool{}
	for path, mf := range st.ManagedFiles {
		sopsValues[filepath.Base(path)] = mf.SopsValues
	}
	assert.Equal(t, map[string]bool{
		"pin": true, "plain.txt": false, "app.container": false, "secret:db_password": true,
	}, sopsValues)
}

// A file whose content stays byte-identical while its render switches to a
// SOPS value (a plain value migrated to sops:// unchanged) is a no-op for the
// diff, yet its state record gains the SopsValues flag, so the dashboard
// stops showing its hash.
func TestAgentRefreshesSopsValuesOfUnchangedFile(t *testing.T) {
	t.Parallel()
	files := sopsFleet(dbPasswordSecret, map[string]string{
		"secrets/db.sops.yaml": sopsCorpus(t, "values.sops.yaml"),
		"files/pin.tmpl":       "pin=hunter2-test",
	}, "files/pin.tmpl")
	h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))
	require.NoError(t, h.tick(t))
	assert.False(t, pinSopsValues(t, h), "a plain render holds no SOPS value")

	pushToTestRepo(t, h.bareDir, map[string]string{
		"files/pin.tmpl": `pin={{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
	})
	h.reset()
	require.NoError(t, h.tick(t))
	assert.Empty(t, h.restarts, "nothing changed on disk")
	assert.True(t, pinSopsValues(t, h), "the record follows the render")
}

func pinSopsValues(t *testing.T, h *sopsHarness) bool {
	t.Helper()
	st, err := h.store.Load()
	require.NoError(t, err)
	for path, mf := range st.ManagedFiles {
		if filepath.Base(path) == "pin" {
			return mf.SopsValues
		}
	}
	require.Fail(t, "pin not managed")
	return false
}
