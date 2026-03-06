package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/reconciler"
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

func newBareMocks(t *testing.T) (*mocks.MockSystemdManager, *mocks.MockPodmanClient, *mocks.MockFileWriter) {
	t.Helper()
	return mocks.NewMockSystemdManager(t), mocks.NewMockPodmanClient(t), mocks.NewMockFileWriter(t)
}

// setupApplyMocks configures mocks for a test that expects a successful apply
// (health check + write files + daemon-reload + restart units).
func setupApplyMocks(sys *mocks.MockSystemdManager, pod *mocks.MockPodmanClient, fw *mocks.MockFileWriter) map[string][]byte {
	// Orphan scan at startup calls ListManagedSecrets
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	// Health check
	sys.EXPECT().IsActive(mock.Anything, mock.Anything).Return(true, nil).Maybe()
	sys.EXPECT().GetUnitState(mock.Anything, mock.Anything).Return("active", nil).Maybe()

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
func setupNoopMocks(sys *mocks.MockSystemdManager, pod *mocks.MockPodmanClient) {
	sys.EXPECT().IsActive(mock.Anything, mock.Anything).Return(true, nil).Maybe()
	sys.EXPECT().GetUnitState(mock.Anything, mock.Anything).Return("active", nil).Maybe()
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
	// Dry-run: health checks happen, but no writes/restarts
	setupNoopMocks(sys, pod)
	written := make(map[string][]byte)

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

	// In dry-run, no files should be written
	assert.Empty(t, written, "dry-run should not write files")
}

func TestAgentSkipsFailedSHA(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw := newBareMocks(t)
	// Skipped SHA: health checks only, no writes expected
	setupNoopMocks(sys, pod)
	written := make(map[string][]byte)

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
	st.FailedCount = 3 // maxRetries reached → will be skipped
	require.NoError(t, store.Save(st))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	require.NoError(t, <-errCh)

	// Should NOT have written any files since SHA is permanently failed
	assert.Empty(t, written, "should not write files for permanently failed SHA")
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
	sys.EXPECT().IsActive(mock.Anything, mock.Anything).Return(true, nil).Maybe()
	sys.EXPECT().GetUnitState(mock.Anything, mock.Anything).Return("active", nil).Maybe()
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
