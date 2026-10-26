package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/state"
)

// loopConfig is the Agent config the loop tests start from: test-host
// polling bareDir's master branch every second, with its own secrets
// directory. Callers adjust it before the Agent runs.
func loopConfig(t *testing.T, bareDir string) *agentcfg.Config {
	t.Helper()
	return &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
}

// applyRecord is what the mocks set up by setupApplyMocks saw the Agent do.
// Fields are guarded by mu while the Agent runs.
type applyRecord struct {
	mu       sync.Mutex
	written  map[string][]byte // files, by path
	deleted  []string          // files
	secrets  map[string]string // secrets created, by name (latest content)
	created  []string          // secret names, one per create
	removed  []string          // secrets
	restarts []string          // units
	reloads  int               // daemon-reloads
	// writeErr, when set, fails every file write.
	writeErr error
}

func newApplyRecord() *applyRecord {
	return &applyRecord{written: map[string][]byte{}, secrets: map[string]string{}}
}

// harnessMocks picks the groups of catch-all expectations setupApplyMocks
// sets. The mocks are strict: a call that neither a picked group nor the
// test expects fails the test, so a test picks only the groups its Agent may
// call.
type harnessMocks uint

const (
	mockOrphanScan    harnessMocks = 1 << iota // ListManagedSecrets: no managed secrets
	mockHealth                                 // GetUnitStatus: every unit active and running
	mockFiles                                  // WriteFile, MkdirAll, Remove and DaemonReload
	mockStops                                  // StopUnit
	mockRestarts                               // RestartUnit
	mockStarts                                 // StartUnit
	mockSecrets                                // SecretExists (none exists) and SecretCreate
	mockSecretRemoves                          // SecretRemove

	// mocksNoop takes the calls of a loop that applies nothing: the
	// startup orphan scan and the health checks.
	mocksNoop = mockOrphanScan | mockHealth
	// mocksApply also takes an apply's file writes, stops and restarts.
	mocksApply = mocksNoop | mockFiles | mockStops | mockRestarts
)

// setupApplyMocks sets the groups of catch-all expectations picked by groups
// on the mocks, recording what they see into r.
func setupApplyMocks(r *applyRecord, groups harnessMocks, sys *appliermocks.MockSystemdManager, pod *appliermocks.MockPodmanClient, fw *appliermocks.MockFileWriter) {
	locked := func(f func()) {
		r.mu.Lock()
		defer r.mu.Unlock()
		f()
	}
	if groups&mockOrphanScan != 0 {
		pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	}
	if groups&mockHealth != 0 {
		sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
			Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	}
	if groups&mockFiles != 0 {
		sys.EXPECT().DaemonReload(mock.Anything).RunAndReturn(func(context.Context) error {
			locked(func() { r.reloads++ })
			return nil
		}).Maybe()
		fw.EXPECT().WriteFile(mock.Anything, mock.Anything).RunAndReturn(func(path string, content []byte) (err error) {
			locked(func() {
				if err = r.writeErr; err == nil {
					r.written[path] = content
				}
			})
			return err
		}).Maybe()
		fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
		fw.EXPECT().Remove(mock.Anything).RunAndReturn(func(path string) error {
			locked(func() { r.deleted = append(r.deleted, path) })
			return nil
		}).Maybe()
	}
	if groups&mockStops != 0 {
		sys.EXPECT().StopUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	}
	if groups&mockRestarts != 0 {
		sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, unit string) error {
			locked(func() { r.restarts = append(r.restarts, unit) })
			return nil
		}).Maybe()
	}
	if groups&mockStarts != 0 {
		sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	}
	if groups&mockSecrets != 0 {
		pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
		pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, data []byte, _ bool) error {
				locked(func() {
					r.secrets[name] = string(data)
					r.created = append(r.created, name)
				})
				return nil
			}).Maybe()
	}
	if groups&mockSecretRemoves != 0 {
		pod.EXPECT().SecretRemove(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, name string) error {
			locked(func() { r.removed = append(r.removed, name) })
			return nil
		}).Maybe()
	}
}

// reset forgets what the Agent did so far.
func (r *applyRecord) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.written)
	clear(r.secrets)
	r.deleted, r.created, r.removed, r.restarts, r.reloads = nil, nil, nil, nil, 0
}

// file returns the content written to path, and whether it was written.
func (r *applyRecord) file(path string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	content, ok := r.written[path]
	return content, ok
}

// secret returns the content the secret name was last created with.
func (r *applyRecord) secret(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secrets[name]
}

// creates counts the creates of the secret name.
func (r *applyRecord) creates(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.created {
		if c == name {
			n++
		}
	}
	return n
}

// assertNothingApplied checks that the Agent wrote, deleted, created,
// removed, reloaded and restarted nothing since the last reset.
func (r *applyRecord) assertNothingApplied(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Empty(t, r.written, "nothing is written")
	assert.Empty(t, r.deleted, "no file is removed")
	assert.Empty(t, r.created, "no secret is created")
	assert.Empty(t, r.removed, "no secret is removed")
	assert.Empty(t, r.restarts, "nothing is restarted")
	assert.Zero(t, r.reloads, "systemd is not reloaded")
}

// loopHarness is an Agent for test-host over a Fleet repo, with strict
// mocked system boundaries that record what it does (applyRecord).
//
// The harness sets its catch-all expectations (the groups in mocks) only
// when the first run, tick or seedState starts, after the test's own: a
// mock answers a call with the first expectation that matches it, so a test
// that expects a call on sys, pod or fw sets that expectation before then.
// An expectation set later is shadowed by a matching catch-all.
type loopHarness struct {
	*applyRecord

	a         *Agent
	cfg       *agentcfg.Config // loopConfig; adjust before the first run or tick
	mocks     harnessMocks     // mocksApply; adjust before the first run or tick
	sys       *appliermocks.MockSystemdManager
	pod       *appliermocks.MockPodmanClient
	fw        *appliermocks.MockFileWriter
	repoDir   string
	statePath string
	store     *state.Store
	st        *state.State // after runUntilApplied

	expectOnce sync.Once
	poller     *gitpoll.Poller
	health     *health.Checker
}

// newLoopHarness builds the Agent over the Fleet in bareDir; opts follow the
// harness's own options, so they may replace its mocks.
func newLoopHarness(t *testing.T, bareDir string, opts ...Option) *loopHarness {
	t.Helper()
	return newConfiguredLoopHarness(t, loopConfig(t, bareDir), opts...)
}

// newConfiguredLoopHarness is newLoopHarness for the Agent config cfg.
func newConfiguredLoopHarness(t *testing.T, cfg *agentcfg.Config, opts ...Option) *loopHarness {
	t.Helper()
	h := &loopHarness{
		applyRecord: newApplyRecord(),
		cfg:         cfg,
		repoDir:     filepath.Join(t.TempDir(), "clone"),
		statePath:   filepath.Join(t.TempDir(), "state.json"),
		mocks:       mocksApply,
	}
	h.sys, h.pod, h.fw = newBareMocks(t)
	h.store = state.NewStore(h.statePath)
	h.a = newTestAgent(t, h.cfg, append([]Option{
		WithSystemd(h.sys), WithPodman(h.pod), WithFileWriter(h.fw),
		WithRepoPath(h.repoDir), WithStatePath(h.statePath),
	}, opts...)...)
	return h
}

// expect sets the harness's catch-all expectations, once.
func (h *loopHarness) expect() {
	h.expectOnce.Do(func() { setupApplyMocks(h.applyRecord, h.mocks, h.sys, h.pod, h.fw) })
}

// run runs the Agent loop for d.
func (h *loopHarness) run(t *testing.T, d time.Duration) {
	t.Helper()
	h.expect()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	require.NoError(t, h.a.Run(ctx))
}

// start runs the Agent loop in the background until stop (or a 30s
// timeout); stop waits for it to return.
func (h *loopHarness) start(t *testing.T) (stop func()) {
	t.Helper()
	h.expect()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- h.a.Run(ctx) }()
	return func() {
		t.Helper()
		cancel()
		require.NoError(t, <-errCh)
	}
}

// runUntil runs the Agent loop until cond holds (or fails the test after a
// timeout), then stops it.
func (h *loopHarness) runUntil(t *testing.T, cond func() bool, msgAndArgs ...any) {
	t.Helper()
	stop := h.start(t)
	defer stop()
	assert.Eventually(t, cond, 8*time.Second, 20*time.Millisecond, msgAndArgs...)
}

// runUntilApplied runs the Agent loop until a commit applies and keeps the
// state that records it in h.st.
func (h *loopHarness) runUntilApplied(t *testing.T) {
	t.Helper()
	h.runUntil(t, func() bool {
		st, err := h.store.Load()
		return err == nil && st.AppliedSHA != ""
	})
	var err error
	h.st, err = h.store.Load()
	require.NoError(t, err)
	require.NotEmpty(t, h.st.AppliedSHA, "the first commit applies")
}

// tick runs one tick of the Agent loop, cloning the Fleet on the first.
func (h *loopHarness) tick(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return h.a.tick(ctx, h.gitPoller(t), h.store, h.health)
}

// gitPoller returns the poller tick uses, cloning the Fleet on first use.
func (h *loopHarness) gitPoller(t *testing.T) *gitpoll.Poller {
	t.Helper()
	h.expect()
	if h.poller == nil {
		h.poller = gitpoll.New(h.cfg.RepoURL, h.cfg.RepoBranch, h.repoDir, nil, gitpoll.WithLogger(discardLogger()))
		require.NoError(t, h.poller.Init(t.Context()))
		h.health = health.New(h.a.systemd, health.WithLogger(discardLogger()))
	}
	return h.poller
}

// seedState saves a state that marks the clone's HEAD applied, edited by
// edit (when not nil), so the next tick sees no new commit. With
// manageResolved, the state also manages every file the Fleet resolves to,
// so the tick's diff is empty.
func (h *loopHarness) seedState(t *testing.T, manageResolved bool, edit func(*state.State)) {
	t.Helper()
	initial, err := h.gitPoller(t).Poll(t.Context(), "")
	require.NoError(t, err)
	st := state.NewState()
	if manageResolved {
		resolved, _, err := LoadAndResolveHost(t.Context(), ResolveParams{
			Logger: discardLogger(), RepoPath: h.repoDir, Hostname: h.cfg.Hostname, SecretsDir: h.cfg.SecretsDir,
		})
		require.NoError(t, err)
		UpdateState(st, reconciler.Diff(resolved.Files, st))
	}
	st.MarkApplied(initial.HeadSHA)
	if edit != nil {
		edit(st)
	}
	require.NoError(t, h.store.Save(st))
}

// assertNothingWritten checks that nothing was applied since the last reset
// and, after runUntilApplied, that the applied SHA is still that commit's.
func (h *loopHarness) assertNothingWritten(t *testing.T) {
	t.Helper()
	h.assertNothingApplied(t)
	if h.st == nil {
		return
	}
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, h.st.AppliedSHA, st.AppliedSHA)
}
