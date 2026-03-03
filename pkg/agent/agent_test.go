package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/metrics"
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

func newTestMocks(t *testing.T) (*mocks.MockSystemdManager, *mocks.MockPodmanClient, *mocks.MockFileWriter, map[string][]byte) {
	t.Helper()
	sys := mocks.NewMockSystemdManager(t)
	sys.EXPECT().IsActive(mock.Anything, mock.Anything).Return(true, nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().GetUnitState(mock.Anything, mock.Anything).Return("active", nil).Maybe()

	pod := mocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	pod.EXPECT().RunHealthcheck(mock.Anything, mock.Anything).Return(true, nil).Maybe()
	pod.EXPECT().GetPodState(mock.Anything, mock.Anything).Return("Running", nil).Maybe()

	written := make(map[string][]byte)
	fw := mocks.NewMockFileWriter(t)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) error {
		written[path] = content
		return nil
	}).Maybe()
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()

	return sys, pod, fw, written
}

func TestAgentFullCycle(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := filepath.Join(t.TempDir(), "state")
	statePath := filepath.Join(stateDir, "state.json")

	metrics.Register()

	sys, pod, fw, written := newTestMocks(t)

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
	assert.Contains(t, written, "/etc/containers/systemd/internal.network")

	// Verify state was saved
	_, err := os.Stat(statePath)
	assert.NoError(t, err, "state file should be created")
}

func TestAgentDryRun(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw, written := newTestMocks(t)

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

//nolint:funlen // integration test with extensive setup
func TestAgentSkipsFailedSHA(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw, written := newTestMocks(t)

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
	st := &state.State{
		ManagedFiles: make(map[string]string),
		FailedSHA:    head.Hash().String(),
		FailedCount:  3, // maxRetries reached → will be skipped
	}
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

//nolint:funlen // integration test with extensive setup
func TestAgentRetriesFailedSHA(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys, pod, fw, written := newTestMocks(t)

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
	st := &state.State{
		ManagedFiles: make(map[string]string),
		FailedSHA:    head.Hash().String(),
		FailedCount:  1, // only 1 failure, will retry
	}
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
	assert.Contains(t, written, "/etc/containers/systemd/internal.network")
}
