package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/mqtt"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// initFleetRepo commits files (Fleet-relative path → content) to a new repo
// and returns a bare clone of it, to use as RepoURL, and its HEAD SHA.
func initFleetRepo(t *testing.T, files map[string]string) (bareDir, headSHA string) {
	t.Helper()
	bareDir = filepath.Join(t.TempDir(), "bare.git")
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
	head, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@test.com"}})
	require.NoError(t, err)
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return bareDir, head.String()
}

// networkFleet is a Fleet whose test-host gets one network quadlet (not a
// template).
func networkFleet() map[string]string {
	return map[string]string{
		"fleet.yml":                          "images:\n  traefik: \"traefik:v3\"\nports:\n  alloy_http: 12345\n  picolet_system_metrics: 9418\n",
		"assignments.yml":                    "base:\n  paths:\n    - quadlets/networks/internal.network\n",
		"hosts/test-host/host.yml":           "hostname: test-host\nexternal_hostname: test-host.ts.net\nrole: server\nfeatures: []\n",
		"quadlets/networks/internal.network": "[Network]\nInternal=true\n",
	}
}

// hookFleet is a Fleet whose only service bundle declares a signal hook
// bound to app.service.
func hookFleet() map[string]string {
	return map[string]string{
		"fleet.yml":                           "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":                     "base:\n  services:\n    - app\n",
		"hosts/test-host/host.yml":            "hostname: test-host\nrole: server\nfeatures: []\n",
		"services/app/secrets/app_config.yml": "v: 1\n",
		"services/app/picolet.yml": `hooks:
  - name: app-sighup
    secrets: [app_config]
    unit: app.service
    action: signal
    container: app
    signal: HUP
    on_failure: keep_running
`,
	}
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

// newMockedAgent is newTestAgent over fresh strict mocks of the system
// boundaries, which it returns for the test's expectations.
func newMockedAgent(t *testing.T, cfg *agentcfg.Config, opts ...Option) (*Agent, *appliermocks.MockSystemdManager, *appliermocks.MockPodmanClient, *appliermocks.MockFileWriter) {
	t.Helper()
	sys, pod, fw := newBareMocks(t)
	return newTestAgent(t, cfg, append([]Option{WithSystemd(sys), WithPodman(pod), WithFileWriter(fw)}, opts...)...), sys, pod, fw
}

func newTestAgent(t *testing.T, cfg *agentcfg.Config, opts ...Option) *Agent {
	t.Helper()
	// The test logger goes first so a caller's WithLogger wins.
	opts = append([]Option{WithLogger(newTestLogger(t))}, opts...)
	opts = append(opts, WithLockPath(filepath.Join(t.TempDir(), "reconciliation.lock")))
	return New(cfg, opts...)
}

func TestApplyWithRollbackReturnsRetryableHookErrorsWithoutRollback(t *testing.T) {
	t.Parallel()
	a, sys, pod, _ := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

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
	}}, nil, nil, "", "", nil)

	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
	require.NotNil(t, result)
	require.Len(t, result.RetryableErrors, 1)
	assert.Equal(t, []string{"app-sighup"}, result.PendingHookNames)
}

func TestReconcileOnceSavesPartialStateOnApplyIncomplete(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", `images: {}
ports: {picolet_system_metrics: 9418}
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

	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()}
	a, sys, pod, _ := newMockedAgent(t, cfg, WithRepoPath(repoDir))
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("shared-secret-value\n"), false).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

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
	a, sys, pod, _ := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)

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

	_, err := a.retryPendingHooks(t.Context(), resolved, st, store, changeset, refSecretsCounts{}, nil)
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
	a, sys, _, fw := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(assert.AnError)

	result, err := a.applyWithRollback(t.Context(), "sha", &reconciler.Changeset{
		Changes: []reconciler.Change{{
			DestPath:    "/etc/containers/systemd/app.container",
			Category:    "container",
			Action:      reconciler.ActionUpdate,
			NewContent:  "[Container]\nImage=app\n",
			ServiceName: "app.service",
		}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, nil, nil, nil, "", "", nil)

	// A failed unit restart yields ErrApplyIncomplete with a non-nil result;
	// a rollback would instead return (nil, errRollbackPerformed).
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
	require.NotNil(t, result)
	assert.Equal(t, []string{"app.service"}, result.FailedRestartUnits)
}

// A failing build is a failed apply, not a pending restart: the deployed
// inputs are rolled back through the existing path and the error counts
// toward the failed-commit gate.
func TestApplyWithRollbackRollsBackFailedBuild(t *testing.T) {
	t.Parallel()
	containerfile := filepath.Join(t.TempDir(), "Containerfile")
	require.NoError(t, os.WriteFile(containerfile, []byte("FROM alpine:old\n"), 0o600))
	a, sys, pod, fw := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(containerfile, []byte("FROM alpine:new\n")).Return(nil).Once()
	pod.EXPECT().ImageID(mock.Anything, "localhost/app").Return("old", nil).Twice()
	sys.EXPECT().RunBuildUnit(mock.Anything, "app-build.service").Return(assert.AnError)
	fw.EXPECT().WriteFile(containerfile, []byte("FROM alpine:old\n")).Return(nil).Once()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)

	result, err := a.applyWithRollback(t.Context(), "sha", &reconciler.Changeset{
		Changes: []reconciler.Change{
			{
				DestPath: "/etc/containers/systemd/picolet/app.build", Category: config.CategoryBuild, Action: reconciler.ActionNoop,
				NewContent:  "[Build]\nImageTag=localhost/app\nFile=" + containerfile + "\nSetWorkingDirectory=file\n",
				ServiceName: "app-build.service",
			},
			{DestPath: containerfile, Category: config.CategoryFile, Action: reconciler.ActionUpdate, NewContent: "FROM alpine:new\n", RelPath: "app/Containerfile"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1, reconciler.ActionNoop: 1},
	}, nil, nil, map[string]status.UnitDependencies{"app.service": {Requires: []string{"app-build.service"}}}, "", "", nil)

	require.ErrorIs(t, err, errRollbackPerformed)
	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, result)
}

func TestApplyWithRollbackCompletesWhenUnmanagedHookUnitRestartFails(t *testing.T) {
	t.Parallel()
	a, sys, pod, _ := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "nginx.service").Return(assert.AnError)

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
	}}, nil, nil, "", "", nil)

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
	writeTestFile(t, repoDir, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
	writeTestFile(t, repoDir, "assignments.yml", `base:
  paths:
    - quadlets/networks/internal.network
`)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, repoDir, "quadlets/networks/internal.network", "[Network]\nInternal=true\n")

	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()}
	a, sys, _, fw := newMockedAgent(t, cfg, WithRepoPath(repoDir))
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(assert.AnError)

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

	_, err := a.finalizeApply(t.Context(), "head-sha", st, store, changeset, &applier.ApplyResult{}, nil, refSecretsCounts{}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, st.PendingUnits, "pending record for a unit removed from the fleet must be pruned")
}

func TestRetryPendingHooksDropsStaleNamesAndKeepsFailures(t *testing.T) {
	t.Parallel()
	a, sys, pod, _ := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)

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

	_, err := a.retryPendingHooks(t.Context(), resolved, st, store, changeset, refSecretsCounts{}, nil)
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

func TestAgentFullCycle(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())
	statePath := filepath.Join(t.TempDir(), "state", "state.json")

	metrics.Register(nil)

	h := newLoopHarness(t, bareDir, WithStatePath(statePath))
	h.run(t, 5*time.Second)

	// Verify that files were written
	assert.Contains(t, h.written, "/etc/containers/systemd/picolet/internal.network")

	// Verify state was saved
	_, err := os.Stat(statePath)
	assert.NoError(t, err, "state file should be created")
}

func TestAgentDryRun(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	// Dry-run: health checks happen, but no writes/restarts.
	h := newLoopHarness(t, bareDir, WithDryRun(true))
	h.mocks = mocksNoop
	h.run(t, 3*time.Second)
	h.assertNothingApplied(t)
}

func TestAgentRetriesFailedSHA(t *testing.T) {
	t.Parallel()
	bareDir, headSHA := initFleetRepo(t, networkFleet())
	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = 100 * time.Millisecond

	// Pre-seed state with only 1 failure — below maxRetries, should retry
	st := state.NewState()
	st.FailedSHA = headSHA
	st.FailedCount = 1 // only 1 failure, will retry
	require.NoError(t, h.store.Save(st))

	h.run(t, 3*time.Second)

	// Should have written files (reconciliation proceeded)
	assert.Contains(t, h.written, "/etc/containers/systemd/picolet/internal.network")
}

func TestAgentDeletionCycle(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)

	// Phase 1: run the agent to initialize the clone and apply the first cycle
	h.runUntilApplied(t)

	assert.Contains(t, h.written, "/etc/containers/systemd/picolet/internal.network",
		"first cycle should have written the network file")
	st := h.st
	require.Contains(t, st.ManagedFiles, "/etc/containers/systemd/picolet/internal.network")

	// Phase 2: simulate repo change — clear assignments on disk.
	// loadAndResolve reads os.DirFS(repoPath) directly; no git operations needed.
	writeTestFile(t, h.repoDir, "assignments.yml", "base: {}\n")

	// Phase 3: deletion reconcile
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reconcileCancel()

	result, err := h.a.ReconcileOnce(reconcileCtx, "delete-sha", st, h.store)
	require.NoError(t, err)
	assert.True(t, result.HasChanges)
	assert.Equal(t, 1, result.Summary[reconciler.ActionDelete])
	require.NotNil(t, result.ApplyResult)
	assert.Empty(t, result.ApplyResult.Errors)

	assert.Contains(t, h.deleted, "/etc/containers/systemd/picolet/internal.network",
		"network file should have been passed to FileWriter.Remove")

	// Verify state is updated: deleted file must be removed
	st2, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, "delete-sha", st2.AppliedSHA)
	assert.Empty(t, st2.ManagedFiles, "state should have no managed files after deleting all")
}

func TestReconcileNoChangesNewSHAMarksApplied(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", `images: {}
ports: {picolet_system_metrics: 9418}
`)
	writeTestFile(t, repoDir, "assignments.yml", `base:
  paths:
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
	resolved, _, err := LoadAndResolveHost(t.Context(), ResolveParams{Logger: discardLogger(), RepoPath: repoDir, Hostname: cfg.Hostname, SecretsDir: cfg.SecretsDir})
	require.NoError(t, err)
	files := resolved.Files

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
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop | mockFiles
	// WriteFile always fails to trigger rollback
	h.writeErr = fmt.Errorf("simulated disk error")
	h.run(t, 5*time.Second)
	assert.Empty(t, h.restarts, "nothing is restarted")

	// Verify state was saved with failure info (not successful apply)
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.NotEmpty(t, st.FailedSHA, "failed SHA should be recorded after apply failure")
	assert.Empty(t, st.AppliedSHA, "applied SHA should not be set after failed apply")
}

// pushToTestRepo commits files (Fleet-relative path → content) on top of
// the bare repo's HEAD, pushes, and returns the new HEAD SHA.
func pushToTestRepo(t *testing.T, bareDir string, files map[string]string) (headSHA string) {
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
	head, err := wt.Commit("update", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	err = repo.Push(&git.PushOptions{})
	require.NoError(t, err)
	return head.String()
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

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	a, _, pod, _ := newMockedAgent(t, cfg, WithStatePath(statePath))
	// Strict expectation (no .Maybe()): if scanOrphans skips the scan
	// (e.g. guard on empty ManagedFiles), ListManagedSecrets is never called
	// and mockery fails the test during cleanup.
	pod.EXPECT().ListManagedSecrets(mock.Anything).Return(nil, nil)
	// System dirs don't exist in test env → file scans are no-ops, but
	// DaemonReload won't be called (no files removed).

	a.scanOrphans(context.Background(), store)
}

func TestAgentPauseSkipsReconciliation(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	mqttMock := agentmocks.NewMockMQTTClient(t)
	mqttMock.EXPECT().Start(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mqttMock.EXPECT().PublishStatus(mock.Anything, mock.Anything).Return(nil).Maybe()
	mqttMock.EXPECT().Close(mock.Anything).Maybe()

	h := newLoopHarness(t, bareDir, WithMQTT(mqttMock))
	h.mocks = mocksNoop
	// Health checks still run when paused; no writes expected.
	h.a.paused.Store(true)
	h.run(t, 3*time.Second)
	h.assertNothingApplied(t)
}

func TestAgentMQTTStatusPublished(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

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

	h := newLoopHarness(t, bareDir, WithMQTT(mqttMock))
	h.cfg.PollInterval = time.Hour // prevent second tick
	stop := h.start(t)
	select {
	case <-statusCh:
		// got a status with applied SHA
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for MQTT status publish with applied SHA")
	}
	stop()

	assert.NotEmpty(t, capturedStatus.AppliedSHA, "AppliedSHA should be set after successful reconciliation")
	assert.False(t, capturedStatus.LastSuccessfulReconciliation.IsZero(), "LastSuccessfulReconciliation should be non-zero")
	assert.False(t, capturedStatus.Paused, "should not be paused")
}

func TestLastSuccessfulReconciliationSeededFromState(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = time.Hour

	// Pre-seed state with a known LastSuccessfulReconciliationAt timestamp.
	seededTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	st := state.NewState()
	st.AppliedSHA = "abc123"
	st.LastSuccessfulReconciliationAt = seededTime
	require.NoError(t, h.store.Save(st))

	// Pause BEFORE running — tick() seeds from state then returns early at the pause
	// check, without entering the reconciliation or noop paths.
	h.a.paused.Store(true)

	// Wait for the first tick to complete (agent becomes ready).
	stop := h.start(t)
	defer stop()
	require.Eventually(t, func() bool { return h.a.ready.Load() }, 5*time.Second, 50*time.Millisecond)

	// The gauge should have been seeded from the pre-existing state at the start of tick(),
	// even though a reconciliation may have updated it afterwards. The important thing is
	// that it's not zero — confirming the seeding path works.
	got := testutil.ToFloat64(metrics.LastSuccessfulReconciliation)
	assert.Greater(t, got, float64(0), "LastSuccessfulReconciliation should not be zero after tick with pre-seeded state")
	assert.GreaterOrEqual(t, got, float64(seededTime.Unix()),
		"LastSuccessfulReconciliation should be at least the seeded timestamp")
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
  picolet_system_metrics: 9418
`)
	writeTestFile(t, repoDir, filepath.Join(subDir, "assignments.yml"), `base:
  paths:
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
	resolved, _, err := LoadAndResolveHost(t.Context(), ResolveParams{Logger: discardLogger(), RepoPath: fleetPath, Hostname: "test-host", SecretsDir: t.TempDir()})
	require.NoError(t, err)
	files := resolved.Files
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
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "fleet.yml"), []byte("images: {}\nports: {picolet_system_metrics: 9418}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "assignments.yml"), []byte(`base:
  paths:
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
	a := New(&agentcfg.Config{Hostname: "test-host"}, WithLogger(newTestLogger(t)),
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
	assert.Empty(t, snap.Host.User, "status keeps host.yml's user: as written; empty for the rootful Host")
	// The metric alone substitutes root: machine defaults to the hostname and
	// the empty user becomes user="root".
	require.NoError(t, testutil.CollectAndCompare(metrics.NewHostInfoCollector(store), strings.NewReader(`
# HELP picolet_host_info Resolved host metadata (value=1).
# TYPE picolet_host_info gauge
picolet_host_info{machine="test-host",role="server",user="root"} 1
`), "picolet_host_info"))
}

func TestTickDoesNotCreateDeploymentForUnchangedSHAOpRefresh(t *testing.T) {
	t.Parallel()

	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	reporter := agentmocks.NewMockDeploymentReporter(t)
	h := newLoopHarness(t, bareDir, WithDeploymentReporter(reporter))
	h.cfg.OnePassword = &agentcfg.OnePasswordConfig{RefreshInterval: time.Hour}
	h.a.opReader = resolver.SecretRefReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{}, nil
	})
	h.seedState(t, false, nil)

	require.NoError(t, h.tick(t))
	reporter.AssertNotCalled(t, "CreateDeployment", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportInProgress", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportSuccess", mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportFailure", mock.Anything, mock.Anything, mock.Anything)
	reporter.AssertNotCalled(t, "ReportError", mock.Anything, mock.Anything, mock.Anything)
}

func TestTickBypassesNoopGateWhenHooksPending(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, hookFleet())
	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop
	h.pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)

	// Pre-seed state at the current SHA, with the secret already managed and
	// one hook waiting for retry. The diff against this state must be empty;
	// the noop gate must be bypassed because PendingHooks is non-empty.
	h.seedState(t, true, func(st *state.State) { st.PendingHooks = map[string]int{"app-sighup": 1} })

	// The reason this tick ran was a pending-hook retry, not an OP refresh.
	// Snapshot the counter before the tick and assert it advanced by at least
	// one — other parallel tests may also exercise this label, so a strict
	// equality assertion would be racy under -count or t.Parallel.
	beforeRetryLabel := testutil.ToFloat64(metrics.GitPollTotal.WithLabelValues("pending_hook_retry"))

	require.NoError(t, h.tick(t))
	h.assertNothingApplied(t)

	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingHooks, "successful retry should clear pending list")
	assert.Equal(t, 0, loaded.FailedCount, "FailedCount must not be touched on successful retry")

	afterRetryLabel := testutil.ToFloat64(metrics.GitPollTotal.WithLabelValues("pending_hook_retry"))
	assert.GreaterOrEqual(t, afterRetryLabel, beforeRetryLabel+1, "pending-hook retries must increment the pending_hook_retry label, not op_refresh")
}

// pendingUnitState is a seedState edit for the network-only test repo that
// adds one pending unit to a state at the current SHA (empty diff).
func pendingUnitState(t *testing.T) func(*state.State) {
	t.Helper()
	return func(st *state.State) {
		var unit string
		for _, u := range st.ServiceNames {
			unit = u
		}
		require.NotEmpty(t, unit, "fixture must produce at least one managed unit")
		st.PendingUnits = map[string]state.PendingUnit{
			unit: {SHA: st.AppliedSHA, Attempts: 5, FirstFailedAt: time.Now().Add(-time.Hour), LastAttemptAt: time.Now()},
		}
	}
}

// TestTickReportsRetryPendingWhenUnitsPending verifies that a tick with no git
// change but a still-failing unit reports retry_pending — not a clean noop or
// success — and does not advance failure tracking or the last-successful time.
func TestTickReportsRetryPendingWhenUnitsPending(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())
	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mockOrphanScan
	// Unit stays failed; the persisted cooldown (LastAttemptAt now) suppresses
	// a restart, so RestartUnit must not be called.
	h.sys.EXPECT().GetUnitStatus(mock.Anything, mock.AnythingOfType("string")).
		Return(applier.UnitStatus{ActiveState: "failed", SubState: "failed"}, nil).Maybe()
	h.seedState(t, true, pendingUnitState(t))

	preTick, err := h.store.Load()
	require.NoError(t, err)

	before := testutil.ToFloat64(metrics.ReconciliationTotal.WithLabelValues("retry_pending"))
	require.NoError(t, h.tick(t))
	assert.Empty(t, h.restarts, "the cooldown suppresses the restart")

	after := testutil.ToFloat64(metrics.ReconciliationTotal.WithLabelValues("retry_pending"))
	assert.GreaterOrEqual(t, after, before+1, "a pending-unit tick must count as retry_pending")

	loaded, err := h.store.Load()
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
	bareDir, _ := initFleetRepo(t, networkFleet())
	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop
	h.seedState(t, true, pendingUnitState(t))
	require.NoError(t, h.tick(t))

	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingUnits, "a healthy unit must clear its pending record and the change must be persisted")
}

// timerJobState is a seedState edit for the network-only test repo that adds
// a raw .timer + one-shot pair to the managed unit set so the health pass
// queries them.
func timerJobState(st *state.State) {
	st.ServiceNames["/etc/systemd/user/job.service"] = "job.service"
	st.ServiceNames["/etc/systemd/user/job.timer"] = "job.timer"
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
	bareDir, _ := initFleetRepo(t, networkFleet())
	metrics.Register(nil)

	firstStart := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	firstEnd := firstStart.Add(time.Minute)
	secondStart := firstStart.Add(time.Hour)

	h := newLoopHarness(t, bareDir)
	h.mocks = mockOrphanScan
	jobRunning := timerJobStatuses(h.sys, firstStart, firstEnd, secondStart)
	h.seedState(t, true, timerJobState)

	require.NoError(t, h.tick(t))

	runs := h.a.statusStore.Snapshot().Runs
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
	require.NoError(t, h.tick(t))

	runs = h.a.statusStore.Snapshot().Runs
	assert.Equal(t, secondStart, runs["job.service"].StartedAt, "the in-flight run is the last run")
	assert.Equal(t, firstEnd, runs["job.service"].SucceededAt, "a running job keeps its previous last success")
}

// TestTickDropsStalePendingHookNameOnRetry exercises the retryPendingHooks path
// (empty diff, non-empty PendingHooks) for a name that no longer matches
// any hook in the resolved config. RunPendingHooks must mark the name attempted
// so mergePendingHooks drops it and stale entries don't accumulate.
func TestTickDropsStalePendingHookNameOnRetry(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, hookFleet())
	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop
	// Pre-seed state matching the current resolved files (so the diff is empty)
	// AND a pending hook for a *different* secret that did not change in this
	// tick. The pending entry must survive the tick.
	h.seedState(t, true, func(st *state.State) {
		st.PendingHooks = map[string]int{"hook-for-other-secret-not-in-this-bundle": 1}
	})

	require.NoError(t, h.tick(t))
	// No SecretCreate or ContainerKill: this tick has only a state-
	// reconciliation effect (the secret content already matches).
	h.assertNothingApplied(t)

	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.PendingHooks, "stale hook name from a removed config must be dropped after retry")
}

func TestTickDoesNotIncrementFailedCountOnRetryableHookError(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, hookFleet())
	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop
	h.pod.EXPECT().SecretCreate(mock.Anything, "app_config", mock.Anything, false).Return(nil)
	// Hook fails — keep_running default keeps the agent running but should land
	// in PendingHooks rather than incrementing FailedCount.
	h.pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(assert.AnError)
	require.NoError(t, h.store.Save(state.NewState()))

	// tick must return nil — ErrApplyIncomplete is handled internally as
	// "retry pending", not propagated as a reconciliation failure.
	require.NoError(t, h.tick(t))

	loaded, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, 0, loaded.FailedCount, "retryable hook errors must not poison FailedCount")
	assert.Empty(t, loaded.FailedSHA, "FailedSHA must not be set for retryable hook errors")
	assert.Equal(t, map[string]int{"app-sighup": 1}, loaded.PendingHooks)
	assert.NotEmpty(t, loaded.ManagedFiles, "successfully-applied secret must be recorded in state")
	assert.NotEmpty(t, loaded.AppliedSHA, "files were applied — SHA must be marked so gitpoll quiets between retry ticks")
}

func TestApplyWithRollbackRunsPendingHooksAlongsideChangeset(t *testing.T) {
	t.Parallel()
	a, sys, pod, _ := newMockedAgent(t, &agentcfg.Config{Hostname: "host"})
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)
	// NOTE: no DaemonReload expected — secret-only changesets do not set NeedsReload.

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

	result, err := a.applyWithRollback(t.Context(), "head-sha", changeset, hooks, []string{"stale-pending"}, nil, "", "", nil)
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

	got := newTestAgent(t, &agentcfg.Config{}).enforceRetryBudget(pending, hooks)
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

	got := newTestAgent(t, &agentcfg.Config{}).enforceRetryBudget(mergePendingHooks(old, result), hooks)
	assert.Empty(t, got, "count reaches MaxRetries=3, budget drops the entry on the same tick")
}
