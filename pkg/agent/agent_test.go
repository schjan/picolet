package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	agentmocks "github.com/schjan/picolet/mocks/agent"
	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/mqtt"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
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
`)

	// Create assignments.yml
	writeTestFile(t, workDir, "assignments.yml", `base:
  networks:
    - quadlets/networks/internal.network
`)

	// Create host config
	writeTestFile(t, workDir, "hosts/test-host/host.yml", `hostname: test-host
external_hostname: test-host.ts.net
role: server
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

// initTestRepoWithHook creates a bare git repo whose only service bundle
// declares a signal hook bound to app.service. Returned path is the bare repo
// to use as RepoURL.
func initTestRepoWithHook(t *testing.T) string {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	workDir := filepath.Join(t.TempDir(), "work")

	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	writeTestFile(t, workDir, "fleet.yml", `images: {}
ports: {}
`)
	writeTestFile(t, workDir, "assignments.yml", `base:
  services:
    - app
`)
	writeTestFile(t, workDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, workDir, "services/app/secrets/app_config.yml", "v: 1\n")
	writeTestFile(t, workDir, "services/app/picolet.yml", `hooks:
  - name: app-sighup
    secrets: [app_config]
    unit: app.service
    action: signal
    container: app
    signal: HUP
    on_failure: keep_running
`)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	require.NoError(t, err)

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

func newBareMocks(t *testing.T) (*appliermocks.MockSystemdManager, *appliermocks.MockPodmanClient, *appliermocks.MockFileWriter) {
	t.Helper()
	return appliermocks.NewMockSystemdManager(t), appliermocks.NewMockPodmanClient(t), appliermocks.NewMockFileWriter(t)
}

func newTestAgent(t *testing.T, cfg *agentcfg.Config, opts ...Option) *Agent {
	t.Helper()
	opts = append(opts, WithLockPath(filepath.Join(t.TempDir(), "reconciliation.lock")))
	return New(cfg, opts...)
}

func TestApplyWithRollbackReturnsRetryableHookErrorsWithoutRollback(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))
	result, err := a.applyWithRollback(t.Context(), "sha", &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:   "secret:app_config",
			Category:   "secret",
			Action:     reconciler.ActionUpdate,
			NewContent: "new",
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, []config.Hook{{
		Name:      "app-sighup",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionSignal,
		Container: "app",
		Signal:    "HUP",
		OnFailure: config.HookOnFailureKeepRunning,
	}}, nil)

	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
	require.NotNil(t, result)
	require.Len(t, result.RetryableErrors, 1)
	assert.Equal(t, []string{"app-sighup"}, result.PendingHookNames)
}

func TestReconcileOnceSavesPartialStateOnApplyIncomplete(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", `images: {}
ports: {}
`)
	writeTestFile(t, repoDir, "assignments.yml", `base:
  services:
    - app
`)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, repoDir, "services/app/secrets/app_config", "shared-secret-value\n")
	writeTestFile(t, repoDir, "services/app/picolet.yml", `hooks:
  - name: app-sighup
    secrets: [app_config]
    unit: app.service
    action: signal
    container: app
    signal: HUP
    on_failure: keep_running
`)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("shared-secret-value\n"), false).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()}
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw), WithRepoPath(repoDir))

	st := state.NewState()
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(st))

	_, err := a.ReconcileOnce(t.Context(), "head-sha", st, store)
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"app-sighup": 1}, loaded.PendingHooks, "failed hook name persisted for retry")
	assert.NotEmpty(t, loaded.ManagedFiles, "successfully-applied secret recorded so next tick does not re-write it")
	assert.True(t, loaded.LastSuccessfulReconciliationAt.IsZero(),
		"an incomplete apply (pending hook) must not advance the last-successful timestamp")
	// Files applied successfully — mark the SHA so gitpoll stops reporting
	// "Changed" on every retry tick (which would otherwise produce duplicate
	// deployment records and "new git commit detected" log spam).
	assert.Equal(t, "head-sha", loaded.AppliedSHA, "SHA must be marked applied even on partial-apply paths")
}

func TestRetryPendingHooksClearsListOnSuccess(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))

	st := state.NewState()
	st.PendingHooks = map[string]int{"app-sighup": 1}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(st))

	resolved := &resolver.ResolvedHost{
		Hooks: []config.Hook{{
			Name:      "app-sighup",
			Secrets:   []string{"app_config"},
			Unit:      "app.service",
			Action:    config.HookActionSignal,
			Container: "app",
			Signal:    "HUP",
			OnFailure: config.HookOnFailureKeepRunning,
		}},
	}
	changeset := &reconciler.Changeset{Summary: map[reconciler.Action]int{}}

	_, err := a.retryPendingHooks(t.Context(), resolved, st, store, changeset, refSecretsCounts{})
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingHooks, "successful retry should clear pending list")
}

//nolint:funlen // table-driven test with explicit cases for every-tick retry semantics
func TestMergePendingHooksKeepsUnattemptedAndAddsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		old    map[string]int
		result *applier.ApplyResult
		want   map[string]int
	}{
		{
			name:   "attempted+succeeded removed",
			old:    map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{AttemptedHookNames: []string{"hook-a"}},
			want:   nil,
		},
		{
			name:   "empty inputs return nil (not map{} — omitempty must omit)",
			old:    nil,
			result: &applier.ApplyResult{},
			want:   nil,
		},
		{
			name: "attempted+failed_keep_running increments count",
			old:  map[string]int{"hook-a": 2},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a"},
				PendingHookNames:   []string{"hook-a"},
			},
			want: map[string]int{"hook-a": 3},
		},
		{
			name: "previously-pending hook is attempted each tick",
			old:  map[string]int{"hook-a": 1, "hook-b": 1},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a", "hook-b"}, // both attempted via every-tick retry
				PendingHookNames:   nil,                          // both succeeded
			},
			want: nil,
		},
		{
			name: "previously-pending hook fails again, count increments",
			old:  map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a"},
				PendingHookNames:   []string{"hook-a"},
			},
			want: map[string]int{"hook-a": 2},
		},
		{
			name: "new failure added even when not previously pending",
			old:  map[string]int{},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-c"},
				PendingHookNames:   []string{"hook-c"},
			},
			want: map[string]int{"hook-c": 1},
		},
		{
			name:   "attempted+fallback_restart removed (restart already scheduled)",
			old:    map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{AttemptedHookNames: []string{"hook-a"}, FallbackRestartedUnits: []string{"app.service"}},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergePendingHooks(tt.old, tt.result)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMergePendingUnitsClearsConvergedAndAddsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)

	tests := []struct {
		name   string
		old    map[string]state.PendingUnit
		result *applier.ApplyResult
		want   map[string]state.PendingUnit
	}{
		{
			name:   "empty inputs return nil",
			old:    nil,
			result: &applier.ApplyResult{},
			want:   nil,
		},
		{
			name:   "restarted unit is cleared",
			old:    map[string]state.PendingUnit{"foo.service": {SHA: "old", Attempts: 3, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{RestartedUnits: []string{"foo.service"}},
			want:   nil,
		},
		{
			name:   "new failure added with attempt count 1",
			old:    nil,
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want:   map[string]state.PendingUnit{"foo.service": {SHA: "head", Attempts: 1, FirstFailedAt: now, LastAttemptAt: now}},
		},
		{
			name:   "repeated failure increments and preserves FirstFailedAt",
			old:    map[string]state.PendingUnit{"foo.service": {SHA: "old", Attempts: 3, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want:   map[string]state.PendingUnit{"foo.service": {SHA: "head", Attempts: 4, FirstFailedAt: earlier, LastAttemptAt: now}},
		},
		{
			name:   "unrelated pending unit carried forward unchanged",
			old:    map[string]state.PendingUnit{"bar.service": {SHA: "old", Attempts: 1, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want: map[string]state.PendingUnit{
				"bar.service": {SHA: "old", Attempts: 1, FirstFailedAt: earlier, LastAttemptAt: earlier},
				"foo.service": {SHA: "head", Attempts: 1, FirstFailedAt: now, LastAttemptAt: now},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergePendingUnits(tt.old, tt.result, "head", now)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyWithRollbackReturnsIncompleteOnFailedUnitRestart(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(assert.AnError)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))
	result, err := a.applyWithRollback(t.Context(), "sha", &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:    "/etc/containers/systemd/app.container",
			Category:    "container",
			Action:      reconciler.ActionUpdate,
			NewContent:  "[Container]\nImage=app\n",
			ServiceName: "app.service",
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, nil, nil)

	// A failed unit restart yields ErrApplyIncomplete with a non-nil result;
	// a rollback would instead return (nil, errRollbackPerformed).
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
	require.NotNil(t, result)
	assert.Equal(t, []string{"app.service"}, result.FailedRestartUnits)
}

func TestApplyWithRollbackCompletesWhenUnmanagedHookUnitRestartFails(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "nginx.service").Return(assert.AnError)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))
	result, err := a.applyWithRollback(t.Context(), "sha", &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:   "secret:app_config",
			Category:   "secret",
			Action:     reconciler.ActionUpdate,
			NewContent: "new",
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, []config.Hook{{
		Name:    "nginx-reload",
		Secrets: []string{"app_config"},
		Unit:    "nginx.service",
		Action:  config.HookActionRestart,
	}}, nil)

	// nginx.service is not a picolet-managed quadlet — it is absent from the
	// changeset's ServiceNames, so health-enforce could never retry it. Its
	// failed restart stays a logged non-retryable error and must not wedge the
	// apply into an incomplete state with no convergence path.
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, []string{"nginx.service"}, result.FailedRestartUnits)
}

func TestApplyIncompleteError(t *testing.T) {
	t.Parallel()
	managed := map[string]struct{}{"app.service": {}}

	tests := []struct {
		name        string
		result      *applier.ApplyResult
		wantErr     bool
		wantMessage string
	}{
		{
			name:    "clean apply returns nil",
			result:  &applier.ApplyResult{},
			wantErr: false,
		},
		{
			name:        "managed restart failure is incomplete",
			result:      &applier.ApplyResult{FailedRestartUnits: []string{"app.service"}},
			wantErr:     true,
			wantMessage: "units failed to restart: [app.service]",
		},
		{
			name:    "unmanaged restart failure alone is complete",
			result:  &applier.ApplyResult{FailedRestartUnits: []string{"nginx.service"}},
			wantErr: false,
		},
		{
			name:        "unmanaged restart failure omitted from incomplete message",
			result:      &applier.ApplyResult{FailedRestartUnits: []string{"nginx.service", "app.service"}},
			wantErr:     true,
			wantMessage: "units failed to restart: [app.service]",
		},
		{
			name:    "retryable hook error is incomplete regardless of managed set",
			result:  &applier.ApplyResult{RetryableErrors: []error{assert.AnError}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := applyIncompleteError(tt.result, managed)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, applier.ErrApplyIncomplete)
			if tt.wantMessage != "" {
				assert.Contains(t, err.Error(), tt.wantMessage)
			}
		})
	}
}

func TestReconcileOnceSavesPendingUnitsOnFailedRestart(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", "images: {}\nports: {}\n")
	writeTestFile(t, repoDir, "assignments.yml", `base:
  networks:
    - quadlets/networks/internal.network
`)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, repoDir, "quadlets/networks/internal.network", "[Network]\nInternal=true\n")

	sys, pod, fw := newBareMocks(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(assert.AnError)

	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()}
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw), WithRepoPath(repoDir))

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st := state.NewState()
	require.NoError(t, store.Save(st))

	_, err := a.ReconcileOnce(t.Context(), "head-sha", st, store)
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Len(t, loaded.PendingUnits, 1)
	for unit, pu := range loaded.PendingUnits {
		assert.Equal(t, 1, pu.Attempts, "unit %s", unit)
		assert.Equal(t, "head-sha", pu.SHA, "unit %s", unit)
		assert.False(t, pu.FirstFailedAt.IsZero(), "unit %s", unit)
	}
	assert.Equal(t, "head-sha", loaded.AppliedSHA, "SHA marked applied on the partial-apply path")
	assert.NotEmpty(t, loaded.ManagedFiles, "applied file recorded so the next tick does not re-write it")
	assert.True(t, loaded.LastSuccessfulReconciliationAt.IsZero(),
		"an incomplete apply must not advance the last-successful timestamp")
}

func TestFinalizeApplyPrunesPendingUnitsForDeletedUnit(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"})
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st := state.NewState()
	st.PendingUnits = map[string]state.PendingUnit{
		"old.service": {SHA: "old", Attempts: 5, FirstFailedAt: time.Now(), LastAttemptAt: time.Now()},
	}
	changeset := &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:    "/etc/containers/systemd/old.container",
			Category:    "container",
			Action:      reconciler.ActionDelete,
			ServiceName: "old.service",
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	}

	_, err := a.finalizeApply("head-sha", st, store, changeset, &applier.ApplyResult{}, nil, refSecretsCounts{}, nil)
	require.NoError(t, err)
	assert.Empty(t, st.PendingUnits, "pending record for a unit removed from the fleet must be pruned")
}

func TestRetryPendingHooksDropsStaleNamesAndKeepsFailures(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))

	st := state.NewState()
	st.PendingHooks = map[string]int{"app-sighup": 1, "removed-hook": 3}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(st))

	resolved := &resolver.ResolvedHost{
		Hooks: []config.Hook{{
			Name:      "app-sighup",
			Secrets:   []string{"app_config"},
			Unit:      "app.service",
			Action:    config.HookActionSignal,
			Container: "app",
			Signal:    "HUP",
			OnFailure: config.HookOnFailureKeepRunning,
		}},
	}
	changeset := &reconciler.Changeset{Summary: map[reconciler.Action]int{}}

	_, err := a.retryPendingHooks(t.Context(), resolved, st, store, changeset, refSecretsCounts{})
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"app-sighup": 2}, loaded.PendingHooks, "removed-hook is dropped, app-sighup remains pending with incremented count")
}

func TestAcquireLockContentionAndRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "picolet.lock")
	release, err := AcquireLock(path)
	require.NoError(t, err)

	_, err = AcquireLock(path)
	require.ErrorIs(t, err, ErrLocked)

	require.NoError(t, release())
	release, err = AcquireLock(path)
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestScanOrphansSkipsCorruptState(t *testing.T) {
	t.Parallel()
	_, pod, fw := newBareMocks(t)
	a := newTestAgent(t, &agentcfg.Config{}, WithPodman(pod), WithFileWriter(fw))
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	a.scanOrphans(context.Background(), state.NewStore(path))
}

// setupApplyMocks configures mocks for a test that expects a successful apply
// (health check + write files + daemon-reload + restart units).
func setupApplyMocks(sys *appliermocks.MockSystemdManager, pod *appliermocks.MockPodmanClient, fw *appliermocks.MockFileWriter) map[string][]byte {
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
func setupNoopMocks(sys *appliermocks.MockSystemdManager, pod *appliermocks.MockPodmanClient) {
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

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

	a := newTestAgent(t, cfg,
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

	a := newTestAgent(t, cfg,
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

	a := newTestAgent(t, cfg,
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

	a := newTestAgent(t, cfg,
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

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

func TestReconcileNoChangesNewSHAMarksApplied(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", `images: {}
ports: {}
`)
	writeTestFile(t, repoDir, "assignments.yml", `base:
  networks:
    - quadlets/networks/internal.network
`)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, repoDir, "quadlets/networks/internal.network", `[Network]
Internal=true
`)

	cfg := &agentcfg.Config{
		Hostname:   "test-host",
		SecretsDir: t.TempDir(),
	}
	files, err := LoadAndResolve(t.Context(), ResolveParams{RepoPath: repoDir, Hostname: cfg.Hostname, SecretsDir: cfg.SecretsDir})
	require.NoError(t, err)

	st := state.NewState()
	UpdateState(st, reconciler.Diff(files, st))
	st.MarkApplied("old-sha")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, store.Save(st))

	a := newTestAgent(t, cfg, WithRepoPath(repoDir))
	result, err := a.ReconcileOnce(t.Context(), "new-sha", st, store)
	require.NoError(t, err)
	require.False(t, result.HasChanges)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "new-sha", loaded.AppliedSHA)
	assert.False(t, loaded.AppliedAt.IsZero(), "new no-op SHA should be persisted as applied")
	assert.True(t, a.statusStore.Snapshot().Bootstrapped)
}

func TestAgentRollbackOnApplyFailure(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

func TestScanOrphansAfterSchemaMigration(t *testing.T) {
	t.Parallel()

	// Simulate a schema migration: state file exists but has empty ManagedFiles.
	// scanOrphans must NOT skip the scan — picolet-owned files from the previous
	// run would remain as permanent orphans.
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath)
	require.NoError(t, store.Save(state.NewState()))

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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

	metrics.Register(nil)

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

	a := newTestAgent(t, cfg,
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
role: server
features: []
`)
	writeTestFile(t, repoDir, filepath.Join(subDir, "quadlets/networks/internal.network"), `[Network]
Internal=true
`)

	// Resolve from the subdirectory path (simulates what Agent.loadAndResolve does with RepoSubDir).
	fleetPath := filepath.Join(repoDir, subDir)
	files, err := LoadAndResolve(t.Context(), ResolveParams{RepoPath: fleetPath, Hostname: "test-host", SecretsDir: t.TempDir()})
	require.NoError(t, err)
	require.NotEmpty(t, files)
	assert.Equal(t, "/etc/containers/systemd/picolet/internal.network", files[0].DestPath)
}

// TestRefreshResolvedSnapshot_ValidationFailure verifies the contract
// described in the dashboard v2 plan: when AnalyzeFiles fails during a noop
// status refresh, the agent does NOT advance the dependency map (so the
// dashboard keeps showing the previous good map) and does NOT bootstrap.
// Validation failure here surfaces the bad repo state without blocking the
// agent from continuing to verify the deployed state on subsequent ticks.
func TestRefreshResolvedSnapshot_ValidationFailure(t *testing.T) {
	t.Parallel()

	// Build a minimal repo where the resolver succeeds but the validator fails.
	// A network file with no [Network] section parses but quadlet conversion
	// will produce no useful unit info; an empty .container file is a stronger
	// failure path: parsing succeeds but quadlet rejects an unspecified Image.
	repoDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "hosts/test-host"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "fleet.yml"), []byte("images: {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "assignments.yml"), []byte(`base:
  containers:
    - quadlets/broken.container
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "hosts/test-host/host.yml"), []byte(`hostname: test-host
role: server
features: []
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "quadlets"), 0o755))
	// Container without Image= — quadlet.ConvertContainer rejects it.
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "quadlets/broken.container"), []byte(`[Container]
ContainerName=broken
`), 0o600))

	store := status.NewStore()
	a := New(&agentcfg.Config{Hostname: "test-host"},
		WithLockPath(filepath.Join(t.TempDir(), "lock")),
		WithStatusStore(store),
		WithRepoPath(repoDir),
	)

	err := a.refreshResolvedSnapshot(context.Background())
	require.Error(t, err, "validation should fail for container without Image")
	require.Contains(t, err.Error(), "validation failed")

	snap := store.Snapshot()
	assert.False(t, snap.Bootstrapped, "validation failure must not bootstrap the store")
	assert.Empty(t, snap.Dependencies, "deps must remain unset on validation failure")
	// Host metadata IS recorded (recordHostMetadata happens before AnalyzeFiles).
	assert.Equal(t, "server", snap.Host.Role)
}

func TestTickDoesNotCreateDeploymentForUnchangedSHAOpRefresh(t *testing.T) {
	t.Parallel()

	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	metrics.Register(nil)

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
	a := newTestAgent(t, cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
		WithDeploymentReporter(reporter),
	)
	a.opReader = resolver.SecretRefReader(func(_ context.Context, _ []string) (map[string]string, error) {
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

func TestTickBypassesNoopGateWhenHooksPending(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepoWithHook(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))
	initial, err := poller.Poll(ctx, "")
	require.NoError(t, err)

	// Pre-seed state at the current SHA, with the secret already managed and
	// one hook waiting for retry. The diff against this state must be empty;
	// the noop gate must be bypassed because PendingHooks is non-empty.
	files, err := LoadAndResolve(ctx, ResolveParams{RepoPath: repoDir, Hostname: "test-host", SecretsDir: cfg.SecretsDir})
	require.NoError(t, err)
	store := state.NewStore(statePath)
	st := state.NewState()
	UpdateState(st, reconciler.Diff(files, st))
	st.MarkApplied(initial.HeadSHA)
	st.PendingHooks = map[string]int{"app-sighup": 1}
	require.NoError(t, store.Save(st))

	// The reason this tick ran was a pending-hook retry, not an OP refresh.
	// Snapshot the counter before the tick and assert it advanced by at least
	// one — other parallel tests may also exercise this label, so a strict
	// equality assertion would be racy under -count or t.Parallel.
	beforeRetryLabel := testutil.ToFloat64(metrics.GitPollTotal.WithLabelValues("pending_hook_retry"))

	healthChecker := health.New(sys)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingHooks, "successful retry should clear pending list")
	assert.Equal(t, 0, loaded.FailedCount, "FailedCount must not be touched on successful retry")

	afterRetryLabel := testutil.ToFloat64(metrics.GitPollTotal.WithLabelValues("pending_hook_retry"))
	assert.GreaterOrEqual(t, afterRetryLabel, beforeRetryLabel+1, "pending-hook retries must increment the pending_hook_retry label, not op_refresh")
}

// tickPendingUnitFixture clones the network-only test repo, pre-seeds state at
// the current SHA (empty diff) with one pending unit, and returns the store plus
// the pending unit's name.
func tickPendingUnitFixture(t *testing.T, poller *gitpoll.Poller, repoDir, statePath, secretsDir string) (*state.Store, string) {
	t.Helper()
	ctx := t.Context()
	initial, err := poller.Poll(ctx, "")
	require.NoError(t, err)

	files, err := LoadAndResolve(ctx, ResolveParams{RepoPath: repoDir, Hostname: "test-host", SecretsDir: secretsDir})
	require.NoError(t, err)
	store := state.NewStore(statePath)
	st := state.NewState()
	UpdateState(st, reconciler.Diff(files, st))
	st.MarkApplied(initial.HeadSHA)

	var unit string
	for _, u := range st.ServiceNames {
		unit = u
	}
	require.NotEmpty(t, unit, "fixture must produce at least one managed unit")
	st.PendingUnits = map[string]state.PendingUnit{
		unit: {SHA: initial.HeadSHA, Attempts: 5, FirstFailedAt: time.Now().Add(-time.Hour), LastAttemptAt: time.Now()},
	}
	require.NoError(t, store.Save(st))
	return store, unit
}

// TestTickReportsRetryPendingWhenUnitsPending verifies that a tick with no git
// change but a still-failing unit reports retry_pending — not a clean noop or
// success — and does not advance failure tracking or the last-successful time.
func TestTickReportsRetryPendingWhenUnitsPending(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	// Unit stays failed; the persisted cooldown (LastAttemptAt now) suppresses
	// a restart, so RestartUnit must not be called.
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname: "test-host", RepoURL: bareDir, RepoBranch: "master",
		PollInterval: time.Second, MetricsPort: 0, SecretsDir: t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx := t.Context()
	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))
	store, _ := tickPendingUnitFixture(t, poller, repoDir, statePath, cfg.SecretsDir)

	preTick, err := store.Load()
	require.NoError(t, err)

	before := testutil.ToFloat64(metrics.ReconciliationTotal.WithLabelValues("retry_pending"))
	healthChecker := health.New(sys)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	after := testutil.ToFloat64(metrics.ReconciliationTotal.WithLabelValues("retry_pending"))
	assert.GreaterOrEqual(t, after, before+1, "a pending-unit tick must count as retry_pending")

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Len(t, loaded.PendingUnits, 1, "the still-failing unit stays pending")
	assert.Equal(t, 0, loaded.FailedCount, "retry_pending must not arm the failed-SHA gate")
	assert.True(t, loaded.LastSuccessfulReconciliationAt.Equal(preTick.LastSuccessfulReconciliationAt),
		"last-successful time must not advance while a unit is failing")
}

// TestTickClearsPendingUnitWhenHealthy verifies that a tick whose health-enforce
// observes a previously-pending unit as healthy clears the pending record.
func TestTickClearsPendingUnitWhenHealthy(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname: "test-host", RepoURL: bareDir, RepoBranch: "master",
		PollInterval: time.Second, MetricsPort: 0, SecretsDir: t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx := t.Context()
	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))
	store, _ := tickPendingUnitFixture(t, poller, repoDir, statePath, cfg.SecretsDir)

	healthChecker := health.New(sys)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingUnits, "a healthy unit must clear its pending record and the change must be persisted")
}

// tickTimerJobFixture clones the network-only test repo, pre-seeds state at the
// current SHA (empty diff) and adds a raw .timer + one-shot pair to the managed
// unit set so the health pass queries them.
func tickTimerJobFixture(t *testing.T, poller *gitpoll.Poller, repoDir, statePath, secretsDir string) *state.Store {
	t.Helper()
	ctx := t.Context()
	initial, err := poller.Poll(ctx, "")
	require.NoError(t, err)

	files, err := LoadAndResolve(ctx, ResolveParams{RepoPath: repoDir, Hostname: "test-host", SecretsDir: secretsDir})
	require.NoError(t, err)
	store := state.NewStore(statePath)
	st := state.NewState()
	UpdateState(st, reconciler.Diff(files, st))
	st.MarkApplied(initial.HeadSHA)
	if st.ServiceNames == nil {
		st.ServiceNames = make(map[string]string)
	}
	st.ServiceNames["/etc/systemd/user/job.service"] = "job.service"
	st.ServiceNames["/etc/systemd/user/job.timer"] = "job.timer"
	require.NoError(t, store.Save(st))
	return store
}

// timerJobStatuses wires GetUnitStatus for one timer-triggered one-shot
// (job.service) and the timer that fires it, returning the flag that switches the
// one-shot to "a new run is in flight": InactiveExitTimestamp moves, the finish
// timestamp does not, and systemd has already reset Result= to "success".
func timerJobStatuses(sys *appliermocks.MockSystemdManager, firstStart, firstEnd, secondStart time.Time) *atomic.Bool {
	var running atomic.Bool
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		RunAndReturn(func(_ context.Context, name string) (applier.UnitStatus, error) {
			switch name {
			case "job.service":
				job := applier.UnitStatus{
					ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot",
					TriggeredBy:       []string{"job.timer"},
					LastRunStartedAt:  firstStart,
					LastRunFinishedAt: firstEnd,
					Result:            "success",
				}
				if running.Load() {
					job.ActiveState, job.SubState = "activating", "start"
					job.LastRunStartedAt = secondStart
				}
				return job, nil
			case "job.timer":
				return applier.UnitStatus{
					ActiveState: "active", SubState: "waiting", UnitFileState: "enabled",
					LastTriggerAt: secondStart,
				}, nil
			}
			return applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil
		}).Maybe()
	return &running
}

// A full tick must record run bookkeeping for the timer-triggered one-shot and
// the timer that fires it, and must keep the derived last-success while a new run
// is in flight — the window where systemd has already reset Result= to "success"
// and has not yet moved the finish timestamp.
func TestTickRecordsTimerOneshotRuns(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepo(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	firstStart := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	firstEnd := firstStart.Add(time.Minute)
	secondStart := firstStart.Add(time.Hour)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	jobRunning := timerJobStatuses(sys, firstStart, firstEnd, secondStart)

	cfg := &agentcfg.Config{
		Hostname: "test-host", RepoURL: bareDir, RepoBranch: "master",
		PollInterval: time.Second, MetricsPort: 0, SecretsDir: t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx := t.Context()
	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))
	store := tickTimerJobFixture(t, poller, repoDir, statePath, cfg.SecretsDir)
	healthChecker := health.New(sys)

	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	runs := a.statusStore.Snapshot().Runs
	require.Contains(t, runs, "job.service")
	assert.Equal(t, firstStart, runs["job.service"].StartedAt)
	assert.Equal(t, firstEnd, runs["job.service"].SucceededAt)
	assert.Equal(t, "success", runs["job.service"].Result)
	assert.Equal(t, secondStart, runs["job.timer"].TriggeredAt)
	for unit := range runs {
		assert.Contains(t, []string{"job.service", "job.timer"}, unit,
			"only timer-triggered one-shots and managed timers are tracked")
	}

	jobRunning.Store(true)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	runs = a.statusStore.Snapshot().Runs
	assert.Equal(t, secondStart, runs["job.service"].StartedAt, "the in-flight run is the last run")
	assert.Equal(t, firstEnd, runs["job.service"].SucceededAt, "a running job keeps its previous last success")
}

// TestTickDropsStalePendingHookNameOnRetry exercises the retryPendingHooks path
// (empty diff, non-empty PendingHooks) for a name that no longer matches
// any hook in the resolved config. RunPendingHooks must mark the name attempted
// so mergePendingHooks drops it and stale entries don't accumulate.
func TestTickDropsStalePendingHookNameOnRetry(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepoWithHook(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	// No SecretCreate or ContainerKill expected: this tick has only a state-
	// reconciliation effect (the secret content already matches).

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))
	initial, err := poller.Poll(ctx, "")
	require.NoError(t, err)

	// Pre-seed state matching the current resolved files (so the diff is empty)
	// AND a pending hook for a *different* secret that did not change in this
	// tick. The pending entry must survive the tick.
	files, err := LoadAndResolve(ctx, ResolveParams{RepoPath: repoDir, Hostname: "test-host", SecretsDir: cfg.SecretsDir})
	require.NoError(t, err)
	store := state.NewStore(statePath)
	st := state.NewState()
	UpdateState(st, reconciler.Diff(files, st))
	st.MarkApplied(initial.HeadSHA)
	st.PendingHooks = map[string]int{"hook-for-other-secret-not-in-this-bundle": 1}
	require.NoError(t, store.Save(st))

	healthChecker := health.New(sys)
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingHooks, "stale hook name from a removed config must be dropped after retry")
}

func TestTickDoesNotIncrementFailedCountOnRetryableHookError(t *testing.T) {
	t.Parallel()
	bareDir := initTestRepoWithHook(t)
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")
	metrics.Register(nil)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", mock.Anything, false).Return(nil)
	// Hook fails — keep_running default keeps the agent running but should land
	// in PendingHooks rather than incrementing FailedCount.
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		MetricsPort:  0,
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poller := gitpoll.New(cfg.RepoURL, cfg.RepoBranch, repoDir, nil)
	require.NoError(t, poller.Init(ctx))

	store := state.NewStore(statePath)
	require.NoError(t, store.Save(state.NewState()))

	healthChecker := health.New(sys)
	// tick must return nil — ErrApplyIncomplete is handled internally as
	// "retry pending", not propagated as a reconciliation failure.
	require.NoError(t, a.tick(ctx, poller, store, healthChecker))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, 0, loaded.FailedCount, "retryable hook errors must not poison FailedCount")
	assert.Empty(t, loaded.FailedSHA, "FailedSHA must not be set for retryable hook errors")
	assert.Equal(t, map[string]int{"app-sighup": 1}, loaded.PendingHooks)
	assert.NotEmpty(t, loaded.ManagedFiles, "successfully-applied secret must be recorded in state")
	assert.NotEmpty(t, loaded.AppliedSHA, "files were applied — SHA must be marked so gitpoll quiets between retry ticks")
}

func TestApplyWithRollbackRunsPendingHooksAlongsideChangeset(t *testing.T) {
	t.Parallel()
	sys, pod, fw := newBareMocks(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)
	// NOTE: no DaemonReload expected — secret-only changesets do not set NeedsReload.

	a := newTestAgent(t, &agentcfg.Config{Hostname: "host"}, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw))

	hooks := []config.Hook{
		{Name: "stale-pending", Secrets: []string{"app_config"}, Unit: "app.service", Action: config.HookActionSignal, Container: "app", Signal: "HUP", OnFailure: config.HookOnFailureKeepRunning},
		// no current-tick hooks for app_rules; verifying pending fires alongside an unrelated changeset
	}
	changeset := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}

	result, err := a.applyWithRollback(t.Context(), "head-sha", changeset, hooks, []string{"stale-pending"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"stale-pending"}, result.AttemptedHookNames,
		"pending hook ran even though its trigger was not in the changeset")
	require.Len(t, result.HookOutcomes, 1)
	assert.Equal(t, "success", result.HookOutcomes[0].Result)
}

func TestPendingHookExhaustsBudgetAfterMaxRetries(t *testing.T) {
	t.Parallel()
	hooks := []config.Hook{{
		Name:       "app-reload",
		Secrets:    []string{"app_config"},
		Unit:       "app.service",
		Action:     config.HookActionSignal,
		Container:  "app",
		Signal:     "HUP",
		OnFailure:  config.HookOnFailureKeepRunning,
		MaxRetries: 3,
	}}
	pending := map[string]int{"app-reload": 3}

	got := enforceRetryBudget(pending, hooks)
	assert.Empty(t, got, "hook exhausted retry budget should be dropped")
}

func TestMergeThenEnforceBudgetDropsAtLimit(t *testing.T) {
	t.Parallel()
	hooks := []config.Hook{{
		Name:       "app-reload",
		Secrets:    []string{"app_config"},
		Unit:       "app.service",
		Action:     config.HookActionSignal,
		Container:  "app",
		Signal:     "HUP",
		OnFailure:  config.HookOnFailureKeepRunning,
		MaxRetries: 3,
	}}

	old := map[string]int{"app-reload": 2}
	result := &applier.ApplyResult{
		AttemptedHookNames: []string{"app-reload"},
		PendingHookNames:   []string{"app-reload"},
	}

	got := enforceRetryBudget(mergePendingHooks(old, result), hooks)
	assert.Empty(t, got, "count reaches MaxRetries=3, budget drops the entry on the same tick")
}
