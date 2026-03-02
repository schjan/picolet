package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
)

// mockSystemd records calls.
type mockSystemd struct {
	reloads   int
	restarted []string
	states    map[string]string
}

func newMockSystemd() *mockSystemd {
	return &mockSystemd{states: make(map[string]string)}
}

func (m *mockSystemd) DaemonReload(context.Context) error {
	m.reloads++
	return nil
}

func (m *mockSystemd) StartUnit(_ context.Context, _ string) error {
	return nil
}

func (m *mockSystemd) RestartUnit(_ context.Context, name string) error {
	m.restarted = append(m.restarted, name)
	return nil
}

func (m *mockSystemd) GetUnitState(_ context.Context, name string) (string, error) {
	s, ok := m.states[name]
	if !ok {
		return "active", nil
	}
	return s, nil
}

//nolint:contextcheck // test mock uses context.Background()
func (m *mockSystemd) IsActive(_ context.Context, name string) (bool, error) {
	s, _ := m.GetUnitState(context.Background(), name)
	return s == "active", nil
}

// mockPodman for testing.
type mockPodman struct {
	secrets map[string][]byte
}

func newMockPodman() *mockPodman {
	return &mockPodman{secrets: make(map[string][]byte)}
}

func (m *mockPodman) SecretExists(_ context.Context, name string) (bool, error) {
	_, ok := m.secrets[name]
	return ok, nil
}

func (m *mockPodman) SecretCreate(_ context.Context, name string, data []byte, _ bool) error {
	m.secrets[name] = data
	return nil
}

func (m *mockPodman) RunHealthcheck(context.Context, string) (bool, error) {
	return true, nil
}

func (m *mockPodman) GetPodState(context.Context, string) (string, error) {
	return "Running", nil
}

// mockWriter records writes.
type mockWriter struct {
	written map[string][]byte
	removed []string
}

func newMockWriter() *mockWriter {
	return &mockWriter{written: make(map[string][]byte)}
}

func (w *mockWriter) WriteFile(path string, content []byte) error {
	w.written[path] = content
	return nil
}

func (w *mockWriter) MkdirAll(string) error { return nil }

func (w *mockWriter) Remove(path string) error {
	w.removed = append(w.removed, path)
	return nil
}

// initTestRepo creates a git repo with picolet config files for a test host.
func initTestRepo(t *testing.T) string {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	workDir := filepath.Join(t.TempDir(), "work")

	repo, err := git.PlainInit(workDir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Create fleet.yml
	writeTestFile(t, workDir, "fleet.yml", `images:
  traefik: "traefik:v3"
ports:
  alloy_http: 12345
prometheus:
  scrape_interval: "15s"
  scrape_timeout: "10s"
  exporter_scrape_interval: "30s"
  retention_time: "35d"
  retention_size: "2GB"
`)

	// Create assignments.yml
	writeTestFile(t, workDir, "assignments.yml", `base:
  networks:
    - quadlets/networks/internal.network
`)

	// Create host config
	writeTestFile(t, workDir, "hosts/test-host/host.yml", `hostname: test-host
ansible_host: test-host.ts.net
pi_type: server
features: []
`)

	// Create a network file (not a template)
	writeTestFile(t, workDir, "quadlets/networks/internal.network", `[Network]
Internal=true
`)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Clone as bare
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	if err != nil {
		t.Fatal(err)
	}

	return bareDir
}

func writeTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentFullCycle(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := filepath.Join(t.TempDir(), "state")
	statePath := filepath.Join(stateDir, "state.json")

	metrics.Register()

	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMockWriter()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0, // disabled
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

	// Run for a couple of ticks
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	// Wait for context to expire
	<-ctx.Done()

	err := <-errCh
	if err != nil {
		t.Fatalf("agent Run error: %v", err)
	}

	// Verify that files were written
	if _, ok := fw.written["/etc/containers/systemd/internal.network"]; !ok {
		t.Error("expected internal.network to be written")
	}

	// Verify state was saved
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file not created: %v", err)
	}
}

func TestAgentDryRun(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	// Register metrics in a fresh registry to avoid duplicate registration
	// (TestAgentFullCycle may have already registered)
	// Skip re-registration — tests share process

	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMockWriter()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
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
	err := <-errCh
	if err != nil {
		t.Fatalf("agent Run error: %v", err)
	}

	// In dry-run, no files should be written
	if len(fw.written) != 0 {
		t.Errorf("dry-run wrote %d files, want 0", len(fw.written))
	}

	// Daemon-reload should not be called
	if sys.reloads != 0 {
		t.Errorf("dry-run triggered %d daemon-reloads, want 0", sys.reloads)
	}
}

//nolint:funlen // integration test with extensive setup
func TestAgentSkipsFailedSHA(t *testing.T) {
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMockWriter()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 100 * time.Millisecond,
		MetricsPort:  0,
	}

	a := New(cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	// Pre-seed state with the current SHA as failed
	// First, clone to get the SHA
	ctx := context.Background()
	cloneDir := filepath.Join(t.TempDir(), "tmp-clone")
	clonedRepo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: bareDir})
	if err != nil {
		t.Fatal(err)
	}
	head, err := clonedRepo.Head()
	if err != nil {
		t.Fatal(err)
	}

	// Write state with FailedSHA
	store := state.NewStore(statePath)
	st := &state.State{
		ManagedFiles: make(map[string]string),
		FailedSHA:    head.Hash().String(),
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	<-ctx.Done()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	// Should NOT have written any files since SHA is marked as failed
	if len(fw.written) != 0 {
		t.Errorf("wrote %d files for failed SHA, want 0", len(fw.written))
	}
}
