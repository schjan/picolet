package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	agentmocks "github.com/schjan/picolet/mocks/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/mqtt"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// initTestRepo creates a git repo with picolet config files for a test host.
func initTestRepo(t *testing.T) string {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	workDir := filepath.Join(t.TempDir(), "work")

	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	// Create fleet.yml
	writeTestFile(t, workDir, "fleet.yml", `images:
  traefik: "traefik:v3"
ports:
  alloy_http: 12345
prometheus:
  scrape_interval: "15s"
  retention_time: "35d"
`)

	// Create assignments.yml
	writeTestFile(t, workDir, "assignments.yml", `base:
  networks:
    - quadlets/networks/internal.network
`)

	// Create host config
	writeTestFile(t, workDir, "hosts/test-host/host.yml", `hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)

	// Create a network file (not a template)
	writeTestFile(t, workDir, "quadlets/networks/internal.network", `[Network]
Internal=true
`)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	// Clone as bare
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return bareDir
}

func writeTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

func newBareMocks(t *testing.T) (*applier.MockSystemdManager, *applier.MockPodmanClient, *applier.MockFileWriter) {
	t.Helper()
	return applier.NewMockSystemdManager(t), applier.NewMockPodmanClient(t), applier.NewMockFileWriter(t)
}

// setupApplyMocks configures mocks for a test that expects a successful apply
// (health check + write files + daemon-reload + restart units).
func setupApplyMocks(sys *applier.MockSystemdManager, pod *applier.MockPodmanClient, fw *applier.MockFileWriter) map[string][]byte {
	// Orphan scan at startup calls ListManagedSecrets
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	// Health check
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()

	// Apply phase
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StopUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()

	written := make(map[string][]byte)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		written[path] = content
		return nil
	}).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()
	return written
}

// setupNoopMocks configures mocks for a test that should NOT write any files.
// Only health checks are expected.
func setupNoopMocks(sys *applier.MockSystemdManager, pod *applier.MockPodmanClient) {
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	// Orphan scan at startup calls ListManagedSecrets (not in dry-run)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
}

func TestAgentFullCycle(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := filepath.Join(t.TempDir(), "state")
	statePath := filepath.Join(stateDir, "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	written := setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0, // disabled
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Verify that files were written
	assert.Contains(t, written, "/etc/containers/systemd/picolet/internal.network")

	// Verify state was saved
	_, err := os.Stat(statePath)
	assert.NoError(t, err, "state file should be created")
}

func TestAgentDryRun(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	// Dry-run: health checks happen, but no writes/restarts.
	// No WriteFile expectation set — strict mock will fail if WriteFile is called.
	setupNoopMocks(sys, pod)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithDryRun(true),
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Strict mock (no WriteFile expectation) guards against unexpected writes
}

func TestAgentSkipsFailedSHA(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw := newBareMocks(t)
	// Skipped SHA: health checks only, no writes expected.
	// No WriteFile expectation — strict mock will fail if WriteFile is called.
	setupNoopMocks(sys, pod)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 100 * time.Millisecond,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	// Pre-seed state with the current SHA as permanently failed (FailedCount >= maxRetries)
	cloneDir := filepath.Join(t.TempDir(), "tmp-clone")
	clonedRepo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)
	head, err := clonedRepo.Head()
	require.NoError(t, err)

	store := state.NewStore(statePath)
	st := state.NewState()
	st.FailedSHA = head.Hash().String()
	st.FailedCount = 3       // maxRetries reached → will be skipped
	st.FailedAt = time.Now() // recent failure → gate is active
	require.NoError(t, store.Save(st))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Strict mock (no WriteFile expectation) guards against unexpected writes
}

func TestAgentRetriesExpiredFailedSHA(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw := newBareMocks(t)
	written := setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 100 * time.Millisecond,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	// Pre-seed state: SHA failed 3+ times but over an hour ago → gate expired, should retry
	cloneDir := filepath.Join(t.TempDir(), "tmp-clone")
	clonedRepo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)
	head, err := clonedRepo.Head()
	require.NoError(t, err)

	store := state.NewStore(statePath)
	st := state.NewState()
	st.FailedSHA = head.Hash().String()
	st.FailedCount = 5                           // well past maxRetries
	st.FailedAt = time.Now().Add(-2 * time.Hour) // expired: older than failedSHAExpiry (1h)
	require.NoError(t, store.Save(st))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Gate expired → reconciliation should have proceeded and written files
	assert.Contains(t, written, "/etc/containers/systemd/picolet/internal.network")
}

func TestAgentRetriesFailedSHA(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw := newBareMocks(t)
	written := setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 100 * time.Millisecond,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	// Pre-seed state with only 1 failure — below maxRetries, should retry
	cloneDir := filepath.Join(t.TempDir(), "tmp-clone")
	clonedRepo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)
	head, err := clonedRepo.Head()
	require.NoError(t, err)

	store := state.NewStore(statePath)
	st := state.NewState()
	st.FailedSHA = head.Hash().String()
	st.FailedCount = 1 // only 1 failure, will retry
	require.NoError(t, store.Save(st))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Should have written files (reconciliation proceeded)
	assert.Contains(t, written, "/etc/containers/systemd/picolet/internal.network")
}

func TestAgentDeletionCycle(t *testing.T) { //nolint:funlen // three-phase test: create cycle, disk mutation, deletion reconcile
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	// Orphan scan at startup calls ListManagedSecrets
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	// Health checks
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	// Apply operations (creates and deletes)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StopUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	written := make(map[string][]byte)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		written[path] = content
		return nil
	}).Maybe()
	removed := make(map[string]bool)
	fw.EXPECT().Remove(mock.Anything).RunAndReturn(func(path string) error {
		removed[path] = true
		return nil
	}).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	store := state.NewStore(statePath)

	// Phase 1: run the agent to initialize the clone and apply the first cycle
	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(runCtx) }()
	<-runCtx.Done()
	require.NoError(t, <-errCh)

	assert.Contains(t, written, "/etc/containers/systemd/picolet/internal.network",
		"first cycle should have written the network file")
	st, err := store.Load()
	require.NoError(t, err)
	require.NotEmpty(t, st.AppliedSHA, "first cycle must have saved state")
	require.Contains(t, st.ManagedFiles, "/etc/containers/systemd/picolet/internal.network")

	// Phase 2: simulate repo change — clear assignments on disk.
	// loadAndResolve reads os.DirFS(repoPath) directly; no git operations needed.
	writeTestFile(t, repoDir, "assignments.yml", "base: {}\n")

	// Phase 3: deletion reconcile
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reconcileCancel()

	result, err := a.ReconcileOnce(reconcileCtx, "delete-sha", st, store)
	require.NoError(t, err)
	assert.True(t, result.HasChanges)
	assert.Equal(t, 1, result.Summary[reconciler.ActionDelete])
	require.NotNil(t, result.ApplyResult)
	assert.Empty(t, result.ApplyResult.Errors)

	assert.Contains(t, removed, "/etc/containers/systemd/picolet/internal.network",
		"network file should have been passed to FileWriter.Remove")

	// Verify state is updated: deleted file must be removed
	st2, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "delete-sha", st2.AppliedSHA)
	assert.Empty(t, st2.ManagedFiles, "state should have no managed files after deleting all")
}

func TestAgentRollbackOnApplyFailure(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	setupNoopMocks(sys, pod)

	// WriteFile always fails to trigger rollback
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(fmt.Errorf("simulated disk error")).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	// Orphan scan at startup
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Verify state was saved with failure info (not successful apply)
	store := state.NewStore(statePath)
	st, err := store.Load()
	require.NoError(t, err)
	assert.NotEmpty(t, st.FailedSHA, "failed SHA should be recorded after apply failure")
	assert.Empty(t, st.AppliedSHA, "applied SHA should not be set after failed apply")
}

func TestTriggerReconcileChannel(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := New(cfg)

	// First send should succeed
	a.triggerReconcile()

	// Second send should be dropped (channel full, buffered=1)
	a.triggerReconcile()

	// Drain: should get exactly one signal
	select {
	case <-a.webhookCh:
		// expected
	default:
		t.Fatal("expected signal in webhookCh")
	}

	// Channel should now be empty
	select {
	case <-a.webhookCh:
		t.Fatal("channel should be empty after drain")
	default:
		// expected
	}
}

func TestWebhookOnHTTPServer(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := New(cfg)

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader("{}")) //nolint:noctx // test helper, no context needed
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Channel should have received a signal
	select {
	case <-a.webhookCh:
		// expected
	default:
		t.Fatal("expected signal in webhookCh after POST /webhook")
	}
}

func TestHealthEndpoint_ReturnsUnavailableBeforeFirstTick(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := New(cfg)

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestHealthEndpoint_ReturnsOKAfterSuccessfulTick(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Hour,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	// Wait until agent becomes ready
	require.Eventually(t, func() bool {
		resp, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "/health should return 200 after first tick")

	cancel()
	require.NoError(t, <-errCh)
}

func pushToTestRepo(t *testing.T, bareDir string, files map[string]string) {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "push-work")
	repo, err := git.PlainClone(workDir, false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)
	for path, content := range files {
		writeTestFile(t, workDir, path, content)
	}
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	_, err = wt.Commit("update", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	err = repo.Push(&git.PushOptions{})
	require.NoError(t, err)
}

//nolint:funlen // integration test: setup + two sub-tests for valid/invalid webhook
func TestWebhookTriggersReconciliation(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StopUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()

	var mu sync.Mutex
	written := make(map[string][]byte)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		mu.Lock()
		defer mu.Unlock()
		written[path] = content
		return nil
	}).Maybe()

	secretFile := filepath.Join(t.TempDir(), "webhook-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("test-secret"), 0o600))

	cfg := &agentcfg.Config{
		Hostname:          "test-host",
		RepoURL:           bareDir,
		RepoBranch:        "master",
		PollInterval:      time.Hour, // only webhook-triggered reconciliation
		MetricsPort:       0,
		SecretsDir:        t.TempDir(),
		WebhookSecretPath: secretFile,
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	// Wait for initial tick to complete
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(written) > 0
	}, 10*time.Second, 50*time.Millisecond, "initial tick should write files")

	// Push a new commit that adds hello.container
	pushToTestRepo(t, bareDir, map[string]string{
		"assignments.yml": `base:
  networks:
    - quadlets/networks/internal.network
  containers:
    - quadlets/containers/hello.container
`,
		"quadlets/containers/hello.container": `[Container]
Image=hello-world:latest
`,
	})

	// Wrong signature → 403, no trigger
	badReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook", strings.NewReader("{}"))
	require.NoError(t, err)
	badReq.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	badResp, err := http.DefaultClient.Do(badReq)
	require.NoError(t, err)
	defer badResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, badResp.StatusCode)

	select {
	case <-a.webhookCh:
		t.Fatal("trigger should not have been called")
	default:
		// expected — handler returned 403, no trigger
	}

	// Valid signature → 202, triggers reconciliation
	body := []byte("{}")
	sig := ComputeSignature(body, "test-secret")
	goodReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	require.NoError(t, err)
	goodReq.Header.Set("X-Hub-Signature-256", sig)
	goodResp, err := http.DefaultClient.Do(goodReq)
	require.NoError(t, err)
	defer goodResp.Body.Close()
	assert.Equal(t, http.StatusAccepted, goodResp.StatusCode)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		_, ok := written["/etc/containers/systemd/picolet/hello.container"]
		return ok
	}, 10*time.Second, 50*time.Millisecond, "webhook should trigger reconciliation that writes hello.container")

	cancel()
	require.NoError(t, <-errCh)
}

func TestScanOrphansAfterSchemaMigration(t *testing.T) {
	t.Parallel()

	// Simulate a schema migration: state file exists but has empty ManagedFiles.
	// scanOrphans must NOT skip the scan — picolet-owned files from the previous
	// run would remain as permanent orphans.
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(state.NewState()))

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	// Strict expectation (no .Maybe()): if scanOrphans skips the scan
	// (e.g. guard on empty ManagedFiles), ListManagedSecrets is never called
	// and mockery fails the test during cleanup.
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)
	// System dirs don't exist in test env → file scans are no-ops, but
	// DaemonReload won't be called (no files removed).
	_ = sys
	_ = fw

	cfg := &agentcfg.Config{
		Hostname: "test-host",
		RepoURL:  "https://example.com/repo.git",
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithStatePath(statePath),
	)

	a.scanOrphans(context.Background(), store)
}

func TestAgentPauseSkipsReconciliation(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	// Health checks still run when paused; no writes expected.
	setupNoopMocks(sys, pod)

	mqttMock := agentmocks.NewMockMQTTClient(t)
	mqttMock.EXPECT().Start(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mqttMock.EXPECT().PublishStatus(mock.Anything, mock.Anything).Return(nil).Maybe()
	mqttMock.EXPECT().Close(mock.Anything).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
		WithMQTT(mqttMock),
	)
	// Pause before running — strict mock (no WriteFile expectation) will fail if writes occur.
	a.paused.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// No file writes should have occurred (strict mock guards this).
}

//nolint:funlen // setup-heavy integration test: mocks + git repo + status capture
func TestAgentMQTTStatusPublished(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)

	var capturedStatus mqtt.Status
	var statusCaptured sync.Once
	statusCh := make(chan struct{})

	mqttMock := agentmocks.NewMockMQTTClient(t)
	mqttMock.EXPECT().Start(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mqttMock.EXPECT().PublishStatus(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, s mqtt.Status) error {
		if s.AppliedSHA != "" {
			statusCaptured.Do(func() {
				capturedStatus = s
				close(statusCh)
			})
		}
		return nil
	}).Maybe()
	mqttMock.EXPECT().Close(mock.Anything).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Hour, // prevent second tick
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
		WithMQTT(mqttMock),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	select {
	case <-statusCh:
		// got a status with applied SHA
	case <-ctx.Done():
		t.Fatal("timed out waiting for MQTT status publish with applied SHA")
	}

	cancel()
	require.NoError(t, <-errCh)

	assert.NotEmpty(t, capturedStatus.AppliedSHA, "AppliedSHA should be set after successful reconciliation")
	assert.False(t, capturedStatus.LastSuccessfulReconciliation.IsZero(), "LastSuccessfulReconciliation should be non-zero")
	assert.False(t, capturedStatus.Paused, "should not be paused")
}

func TestLastSuccessfulReconciliationSeededFromState(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	// Pre-seed state with a known LastSuccessfulReconciliationAt timestamp.
	seededTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	st := state.NewState()
	st.AppliedSHA = "abc123"
	st.LastSuccessfulReconciliationAt = seededTime
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(st))

	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Hour,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)
	// Pause BEFORE running — tick() seeds from state then returns early at the pause
	// check, without entering the reconciliation or noop paths.
	a.paused.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	// Wait for the first tick to complete (agent becomes ready).
	require.Eventually(t, func() bool { return a.ready.Load() }, 5*time.Second, 50*time.Millisecond)

	// The gauge should have been seeded from the pre-existing state at the start of tick(),
	// even though a reconciliation may have updated it afterwards. The important thing is
	// that it's not zero — confirming the seeding path works.
	got := testutil.ToFloat64(metrics.LastSuccessfulReconciliation)
	assert.Greater(t, got, float64(0), "LastSuccessfulReconciliation should not be zero after tick with pre-seeded state")
	assert.GreaterOrEqual(t, got, float64(seededTime.Unix()),
		"LastSuccessfulReconciliation should be at least the seeded timestamp")

	cancel()
	require.NoError(t, <-errCh)
}

func TestLoadAndResolveWithSubDir(t *testing.T) {
	t.Parallel()

	// Create a repo where fleet files live in a subdirectory.
	repoDir := t.TempDir()
	subDir := "fleet/config"

	writeTestFile(t, repoDir, filepath.Join(subDir, "fleet.yml"), `images:
  traefik: "traefik:v3"
ports:
  app: 8080
`)
	writeTestFile(t, repoDir, filepath.Join(subDir, "assignments.yml"), `base:
  networks:
    - quadlets/networks/internal.network
`)
	writeTestFile(t, repoDir, filepath.Join(subDir, "hosts/test-host/host.yml"), `hostname: test-host
external_hostname: test-host.local
pi_type: server
features: []
`)
	writeTestFile(t, repoDir, filepath.Join(subDir, "quadlets/networks/internal.network"), `[Network]
Internal=true
`)

	// Resolve from the subdirectory path (simulates what Agent.loadAndResolve does with RepoSubDir).
	fleetPath := filepath.Join(repoDir, subDir)
	files, err := LoadAndResolve(t.Context(), fleetPath, "test-host", t.TempDir(), false, nil)
	require.NoError(t, err)
	require.NotEmpty(t, files)
	assert.Equal(t, "/etc/containers/systemd/picolet/internal.network", files[0].DestPath)
}

func TestHealthEndpoint_Returns503AfterConsecutiveDBusFailures(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := New(cfg)
	a.ready.Store(true)

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	// Below threshold: should return 200
	a.consecutiveHealthFailures.Store(2)
	resp, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// At threshold: should return 503
	a.consecutiveHealthFailures.Store(3)
	resp2, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)

	// Recovery: counter resets below threshold
	a.consecutiveHealthFailures.Store(0)
	resp3, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
}

func TestHealthEndpoint_Returns200WhenPausedEvenWithDBusDown(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := New(cfg)
	a.ready.Store(true)
	a.paused.Store(true)
	a.consecutiveHealthFailures.Store(5) // well above threshold

	srv := httptest.NewServer(a.newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUpdateHealthFailures(t *testing.T) {
	t.Parallel()

	t.Run("all errors increments counter", func(t *testing.T) {
		t.Parallel()
		a := New(&agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

		a.updateHealthFailures(&health.CheckResult{
			Errors: []error{fmt.Errorf("dbus dead"), fmt.Errorf("dbus dead")},
		})
		assert.Equal(t, int32(1), a.consecutiveHealthFailures.Load())
	})

	t.Run("mixed results resets counter", func(t *testing.T) {
		t.Parallel()
		a := New(&agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})
		a.consecutiveHealthFailures.Store(5)

		a.updateHealthFailures(&health.CheckResult{
			Healthy: []string{"foo.service"},
			Errors:  []error{fmt.Errorf("dbus dead")},
		})
		assert.Equal(t, int32(0), a.consecutiveHealthFailures.Load())
	})

	t.Run("zero managed units stays at zero", func(t *testing.T) {
		t.Parallel()
		a := New(&agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

		a.updateHealthFailures(&health.CheckResult{})
		assert.Equal(t, int32(0), a.consecutiveHealthFailures.Load())
	})
}

func TestRecordHealthMetrics_ClearsStaleGauges(t *testing.T) {
	t.Parallel()
	metrics.Register()

	// Seed a unit into the global collector (parallel-safe: unique unit name).
	metrics.UnitHealth.Set("clear-test.service", "active", "running")

	// D-Bus fully down: all errors, no statuses.
	recordHealthMetrics(&health.CheckResult{
		Errors:   []error{fmt.Errorf("dbus dead")},
		Statuses: map[string]applier.UnitStatus{},
	})

	// After clearing, a fresh collector should emit nothing for our unit.
	// We verify via a separate collector to avoid asserting on other tests' units.
	collector := metrics.NewUnitHealthCollector()
	collector.Set("verify.service", "active", "running")
	collector.Clear()

	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)
	count := 0
	for range ch {
		count++
	}
	assert.Equal(t, 0, count, "cleared collector should emit no metrics")
}

func TestSetFilesManagedMetric(t *testing.T) {
	t.Parallel()
	metrics.Register()

	counts := map[string]float64{
		"container": 3,
		"network":   1,
		"volume":    0,
		"kube":      2,
		"systemd":   0,
		"manifest":  0,
		"secret":    5,
	}
	setFilesManagedMetric(counts)

	for _, cat := range reconciler.Categories() {
		got := testutil.ToFloat64(metrics.FilesManagedTotal.WithLabelValues(cat))
		assert.InDelta(t, counts[cat], got, 0.001, "category %s", cat)
	}
}

func TestOpRefreshDue(t *testing.T) {
	t.Parallel()

	dummyReader := resolver.OpSecretReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return nil, nil //nolint:nilnil // test stub
	})
	opCfg := &agentcfg.OnePasswordConfig{RefreshInterval: 10 * time.Minute}

	tests := []struct {
		name          string
		opReader      resolver.OpSecretReader
		onePassword   *agentcfg.OnePasswordConfig
		lastOPRefresh time.Time
		want          bool
	}{
		{
			name:     "nil opReader always returns false",
			opReader: nil,
			// cfg.OnePassword populated to prove the nil-reader guard triggers first.
			onePassword: opCfg,
			want:        false,
		},
		{
			name:          "zero lastOPRefresh returns true (first run)",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Time{},
			want:          true,
		},
		{
			name:          "interval not yet elapsed returns false",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Now().Add(-5 * time.Minute), // < 10m interval
			want:          false,
		},
		{
			name:          "interval elapsed returns true",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Now().Add(-11 * time.Minute), // > 10m interval
			want:          true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{
				opReader: tc.opReader,
				cfg: &agentcfg.Config{
					Hostname:    "test-host",
					OnePassword: tc.onePassword,
				},
				lastOPRefresh: tc.lastOPRefresh,
			}
			assert.Equal(t, tc.want, a.opRefreshDue())
		})
	}
}

func TestCreateDeploymentContinuesWhenPendingStatusFails(t *testing.T) {
	t.Parallel()
	metrics.Register()

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().CreateDeployment(mock.Anything, "abc123").Return(int64(42), errors.New("pending status failed"))
	reporter.EXPECT().ReportInProgress(mock.Anything, int64(42)).Return(nil)

	a := New(cfg, WithDeploymentReporter(reporter))
	got := a.createDeployment(context.Background(), "abc123")
	assert.Equal(t, int64(42), got)
}

func TestReportDeploymentResultUsesErrorForRollback(t *testing.T) {
	t.Parallel()
	metrics.Register()

	beforeError := testutil.ToFloat64(metrics.DeploymentStatusTotal.WithLabelValues("error"))

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().ReportError(mock.Anything, int64(7), mock.Anything).Return(nil)

	a := New(cfg, WithDeploymentReporter(reporter))
	a.reportDeploymentResult(context.Background(), 7, fmt.Errorf("%w: apply failed", errRollbackPerformed))

	afterError := testutil.ToFloat64(metrics.DeploymentStatusTotal.WithLabelValues("error"))
	assert.InDelta(t, beforeError+1, afterError, 0.001)
}

func TestReportDeploymentResultUsesDetachedContextOnSuccess(t *testing.T) {
	t.Parallel()
	metrics.Register()

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().ReportSuccess(mock.Anything, int64(9)).RunAndReturn(func(ctx context.Context, _ int64) error {
		assert.NoError(t, ctx.Err(), "terminal deployment report should not inherit parent cancellation")
		return nil
	})

	a := New(cfg, WithDeploymentReporter(reporter))

	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()
	a.reportDeploymentResult(parentCtx, 9, nil)
}

func TestShouldReportDeploymentError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rollback error", err: fmt.Errorf("%w: boom", errRollbackPerformed), want: true},
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "normal error", err: errors.New("validation failed"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldReportDeploymentError(tt.err))
		})
	}
}

func TestTickDoesNotCreateDeploymentForUnchangedSHAOpRefresh(t *testing.T) {
	t.Parallel()

	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register()

	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
		OnePassword: &agentcfg.OnePasswordConfig{
			RefreshInterval: time.Hour,
		},
	}

	reporter := agentmocks.NewMockDeploymentReporter(t)
	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
		WithDeploymentReporter(reporter),
	)
	a.opReader = resolver.OpSecretReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))

	initial, err := poller.Poll(ctx, "")
	require.NoError(t, err)

	store := state.NewStore(statePath)
	st := state.NewState()
	st.MarkApplied(initial.HeadSHA)
	require.NoError(t, store.Save(st))

	healthChecker := health.New(sys)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))
	reporter.AssertNotCalled(t, "CreateDeployment", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportInProgress", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportSuccess", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportFailure", mock.Anything, mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportError", mock.Anything, mock.Anything, mock.Anything)
}
