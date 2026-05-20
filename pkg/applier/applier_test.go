package applier_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testHTTPClient(fn func(*http.Request) int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: fn(req),
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
}

func testHTTPErrorClient(err error) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, err
	})}
}

func TestApplyPhaseOrdering(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "my_secret", []byte("token=abc"), false).Return(nil)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			// Out of order deliberately
			{DestPath: "/etc/containers/systemd/app.container", Category: "container", Action: reconciler.ActionCreate, NewContent: "[Container]\nImage=foo"},
			{DestPath: "/etc/containers/systemd/data.volume", Category: "volume", Action: reconciler.ActionCreate, NewContent: "[Volume]"},
			{DestPath: "secret:my_secret", Category: "secret", Action: reconciler.ActionCreate, NewContent: "token=abc"},
			{DestPath: "/etc/containers/systemd/net.network", Category: "network", Action: reconciler.ActionCreate, NewContent: "[Network]"},
			{DestPath: "/var/lib/picolet/manifests/app/deploy.yml", Category: "manifest", Action: reconciler.ActionCreate, NewContent: "apiVersion: v1"},
			{DestPath: "/etc/containers/systemd/app.kube", Category: "kube", Action: reconciler.ActionCreate, NewContent: "[Kube]\nYaml=foo"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionCreate: 6},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 6, result.Applied)

	// Verify network was written (file category)
	assert.Contains(t, fw.written, "/etc/containers/systemd/net.network")
	// Verify volume was written
	assert.Contains(t, fw.written, "/etc/containers/systemd/data.volume")
}

func TestApplyDryRun(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, true, nil)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/foo.container", Category: "container", Action: reconciler.ActionCreate, NewContent: "content"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionCreate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
	// No actual writes in dry-run
	assert.Empty(t, fw.written)
}

func TestApplyNoop(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/foo.container", Category: "container", Action: reconciler.ActionNoop},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionNoop: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Applied)
}

func TestApplyDelete(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().StopUnit(mock.Anything, "old.service").Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	// RestartUnit must NOT be called for deletes — the unit is gone after daemon-reload
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	const containerPath = "/etc/containers/systemd/old.container"
	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: containerPath, Category: "container", Action: reconciler.ActionDelete, ServiceName: "old.service"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
	assert.Equal(t, []string{containerPath}, fw.removed)
}

func TestApplyDeleteSecret(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretRemove(mock.Anything, "old_secret").Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"old_secret"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:old_secret", Category: "secret", Action: reconciler.ActionDelete},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
	assert.Empty(t, result.Errors)
	assert.Equal(t, int32(0), reloads.Load())
	// Secret deletes go through podman, not file writer
	assert.Empty(t, fw.removed)
}

func TestApplySelfRestart(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	// goroutine fires asynchronously after Apply() returns; may or may not complete before test cleanup
	sys.EXPECT().RestartUnit(mock.Anything, "picolet.service").Return(nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/picolet.container", Category: "container", Action: reconciler.ActionUpdate, NewContent: "[Container]\nImage=ghcr.io/picolet:latest\n", ServiceName: "picolet.service"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.True(t, result.NeedsSelfRestart)
	assert.Contains(t, result.RestartedUnits, "picolet.service")
}

func TestApplySecretReplace(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	// ActionUpdate -> replace=true
	pod.EXPECT().SecretCreate(mock.Anything, "cfg", []byte("new-data"), true).Return(nil)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:cfg", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new-data"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
}

func TestApplyRunsHTTPSecretHookOnce(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	var healthChecks atomic.Int32
	client := testHTTPClient(func(r *http.Request) int {
		switch r.URL.Path {
		case "/reload":
			reloads.Add(1)
			assert.Equal(t, http.MethodGet, r.Method)
			return http.StatusOK
		case "/health":
			healthChecks.Add(1)
			return http.StatusOK
		default:
			return http.StatusNotFound
		}
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config", "app_rules"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodGet,
		URL:       "http://example.test/reload",
		HealthURL: "http://example.test/health",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	}
	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, int32(1), reloads.Load())
	assert.Equal(t, int32(1), healthChecks.Load())
	assert.Empty(t, result.RestartedUnits)
}

func TestApplyHTTPSecretHookFailureKeepsRunningByDefault(t *testing.T) {
	t.Parallel()
	client := testHTTPClient(func(_ *http.Request) int { return http.StatusInternalServerError })

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0].Error(), "reload request")
	require.Len(t, result.RetryableErrors, 1)
	assert.Contains(t, result.RetryableErrors[0].Error(), "reload request")
	assert.Equal(t, []string{"app-reload"}, result.PendingHookNames)
	assert.Empty(t, result.FallbackRestartedUnits)
	assert.Empty(t, result.RestartedUnits)
}

func TestApplyHTTPSecretHookTransportErrorIncludesTarget(t *testing.T) {
	t.Parallel()
	client := testHTTPErrorClient(assert.AnError)

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0].Error(), "performing POST http://example.test/reload")
}

func TestApplyHTTPSecretHookFailureCanRestart(t *testing.T) {
	t.Parallel()
	client := testHTTPClient(func(_ *http.Request) int { return http.StatusInternalServerError })

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureRestart,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	assert.Empty(t, result.RetryableErrors)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}

func TestApplySecretHookStatusFailureCanRestart(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{}, assert.AnError)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureRestart,
	}
	a := applier.New(sys, pod, fw, false, []config.Hook{hook})

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	assert.Empty(t, result.RetryableErrors)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}

func TestApplySignalSecretHook(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().ContainerKill(mock.Anything, "app", "HUP").Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-sighup",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionSignal,
		Container: "app",
		Signal:    "HUP",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	a := applier.New(sys, pod, fw, false, []config.Hook{hook})

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Empty(t, result.RestartedUnits)
}

func TestApplySecretHookSkipsReloadWhenUnitAlreadyRestarting(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "/etc/containers/systemd/app.container", Category: "container", Action: reconciler.ActionUpdate, NewContent: "[Container]\nImage=app\n", ServiceName: "app.service"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, int32(0), reloads.Load())
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}

func TestApplyDeduplicatesHTTPSecretHooksByTarget(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hooks := []config.Hook{
		{
			Name:      "app-config-reload",
			Secrets:   []string{"app_config"},
			Unit:      "app.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			OnFailure: config.HookOnFailureKeepRunning,
		},
		{
			Name:      "app-rules-reload",
			Secrets:   []string{"app_rules"},
			Unit:      "app.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			OnFailure: config.HookOnFailureKeepRunning,
		},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, int32(1), reloads.Load())
	// Both hook names must appear in AttemptedHookNames so a stale pending
	// entry for the dedup-skipped hook gets cleared by mergePendingHooks
	// instead of being carried forward forever.
	assert.ElementsMatch(t, []string{"app-config-reload", "app-rules-reload"}, result.AttemptedHookNames)
}

func TestApplyDoesNotDeduplicateHTTPHooksAcrossUnits(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "sidecar.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().SecretCreate(mock.Anything, "sidecar_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hooks := []config.Hook{
		{
			Name:      "app-reload",
			Secrets:   []string{"app_config"},
			Unit:      "app.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			OnFailure: config.HookOnFailureKeepRunning,
		},
		{
			Name:      "sidecar-reload",
			Secrets:   []string{"sidecar_config"},
			Unit:      "sidecar.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			OnFailure: config.HookOnFailureKeepRunning,
		},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "secret:sidecar_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, int32(2), reloads.Load())
}

func TestApplyRestartSecretHook(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-restart",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionRestart,
		OnFailure: config.HookOnFailureKeepRunning,
	}
	a := applier.New(sys, pod, fw, false, []config.Hook{hook})

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}

func TestApplyDoesNotDeduplicateHTTPHooksByDifferentHealthURL(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := testHTTPClient(func(req *http.Request) int {
		// Both reload (POST) and health (GET) calls hit the test transport;
		// counting all of them is the simplest way to assert "both hooks ran".
		_ = req
		calls.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hooks := []config.Hook{
		{
			Name:      "app-config-reload",
			Secrets:   []string{"app_config"},
			Unit:      "app.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			HealthURL: "http://example.test/health/config",
			OnFailure: config.HookOnFailureKeepRunning,
		},
		{
			Name:      "app-rules-reload",
			Secrets:   []string{"app_rules"},
			Unit:      "app.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			HealthURL: "http://example.test/health/rules",
			OnFailure: config.HookOnFailureKeepRunning,
		},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	// Two reloads + two health checks = 4 HTTP calls. If HealthURL had been
	// excluded from the dedup key, only one hook would fire (2 calls total).
	assert.Equal(t, int32(4), calls.Load())
}

func TestApplyMarksFallbackRestartAsHookFallbackError(t *testing.T) {
	t.Parallel()
	client := testHTTPClient(func(_ *http.Request) int { return http.StatusInternalServerError })

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureRestart,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(context.Background(), &reconciler.Changeset{
		Changes: []reconciler.Change{{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"}},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	if fallback, ok := errors.AsType[*applier.HookFallbackError](result.Errors[0]); ok {
		assert.Equal(t, "app.service", fallback.Unit)
	} else {
		t.Fatalf("expected HookFallbackError, got %T: %v", result.Errors[0], result.Errors[0])
	}
	assert.Equal(t, []string{"app.service"}, result.FallbackRestartedUnits)
	assert.Empty(t, result.RetryableErrors)
	assert.Empty(t, result.PendingHookNames)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
}

func TestRunPendingHooksRetriesNamedHooks(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result := a.RunPendingHooks(context.Background(), []string{"app-reload"})
	assert.Empty(t, result.Errors)
	assert.Empty(t, result.RetryableErrors)
	assert.Empty(t, result.PendingHookNames)
	assert.Equal(t, int32(1), reloads.Load())
}

func TestRunPendingHooksDropsStaleHookNames(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false, nil)

	result := a.RunPendingHooks(context.Background(), []string{"removed-hook"})
	assert.Empty(t, result.Errors)
	assert.Empty(t, result.PendingHookNames)
	// AttemptedHookNames must list every pending name even when no hooks are
	// configured, otherwise mergePendingHooks keeps stale entries forever after
	// the operator removes all hooks from config.
	assert.Equal(t, []string{"removed-hook"}, result.AttemptedHookNames)
}

func TestRunPendingHooksFallbackRestartRestartsUnit(t *testing.T) {
	t.Parallel()
	client := testHTTPClient(func(_ *http.Request) int { return http.StatusInternalServerError })

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureRestart,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result := a.RunPendingHooks(context.Background(), []string{"app-reload"})
	assert.Equal(t, []string{"app.service"}, result.FallbackRestartedUnits)
	assert.Equal(t, []string{"app.service"}, result.RestartedUnits)
	assert.Empty(t, result.RetryableErrors)
	assert.Empty(t, result.PendingHookNames)
}

func TestRunPendingHooksKeepsFailedHookInPending(t *testing.T) {
	t.Parallel()
	client := testHTTPClient(func(_ *http.Request) int { return http.StatusInternalServerError })

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result := a.RunPendingHooks(context.Background(), []string{"app-reload"})
	assert.Len(t, result.RetryableErrors, 1)
	assert.Equal(t, []string{"app-reload"}, result.PendingHookNames)
}

func TestApplyRejectsHookHTTPRedirect(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://elsewhere.test/reload"}},
			Body:       io.NopCloser(http.NoBody),
			Request:    req,
		}, nil
	})}

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	require.Len(t, result.RetryableErrors, 1)
	assert.Contains(t, result.RetryableErrors[0].Error(), "redirect")
	assert.Contains(t, result.RetryableErrors[0].Error(), "elsewhere.test")
	assert.Equal(t, []string{"app-reload"}, result.PendingHookNames)
}

func TestApplyPendsHookOnInactiveUnit(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "inactive", SubState: "dead"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	a := applier.New(sys, pod, fw, false, []config.Hook{hook})

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err) // Apply itself does not return ErrApplyIncomplete; only the agent wrapper does
	require.Len(t, result.RetryableErrors, 1)
	require.ErrorIs(t, result.RetryableErrors[0], applier.ErrUnitNotActive)
	assert.Equal(t, []string{"app-reload"}, result.PendingHookNames)
	require.Len(t, result.HookOutcomes, 1)
	assert.Equal(t, "failed", result.HookOutcomes[0].Result)
}

func TestApplySkippedOutcomeForDedupedHook(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hooks := []config.Hook{
		{Name: "primary", Secrets: []string{"app_config"}, Unit: "app.service", Action: config.HookActionHTTP, Method: http.MethodPost, URL: "http://example.test/reload", OnFailure: config.HookOnFailureKeepRunning},
		{Name: "dedup-peer", Secrets: []string{"app_rules"}, Unit: "app.service", Action: config.HookActionHTTP, Method: http.MethodPost, URL: "http://example.test/reload", OnFailure: config.HookOnFailureKeepRunning},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), reloads.Load())
	require.Len(t, result.HookOutcomes, 2)
	outcomes := map[string]string{}
	for _, o := range result.HookOutcomes {
		outcomes[o.Name] = o.Result
	}
	assert.Equal(t, "success", outcomes["primary"])
	assert.Equal(t, "skipped", outcomes["dedup-peer"])
}

func TestApplySkippedOutcomeForRestartScheduledUnit(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_config", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()
	hook := config.Hook{
		Name:      "app-reload",
		Secrets:   []string{"app_config"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}
	a := applier.New(sys, pod, fw, false, []config.Hook{hook})

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/app.container", Category: "container", Action: reconciler.ActionUpdate, NewContent: "[Container]\nImage=app", ServiceName: "app.service"},
			{DestPath: "secret:app_config", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 2},
	})
	require.NoError(t, err)
	require.Len(t, result.HookOutcomes, 1)
	assert.Equal(t, "skipped", result.HookOutcomes[0].Result)
}

func TestApplyDeduplicatesPendingAgainstCurrentTick(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_rules", []byte("new"), true).Return(nil)
	fw := newMemFileWriter()

	// Two hooks with the SAME dedup key (same unit, same URL, same method).
	// In this tick the changeset triggers only the second one; the first is
	// pending from a previous tick. The second's success must clear the
	// first's pending entry by sharing the dedup map.
	hooks := []config.Hook{
		{Name: "stale-pending", Secrets: []string{"app_config"}, Unit: "app.service", Action: config.HookActionHTTP, Method: http.MethodPost, URL: "http://example.test/reload", OnFailure: config.HookOnFailureKeepRunning},
		{Name: "current-tick", Secrets: []string{"app_rules"}, Unit: "app.service", Action: config.HookActionHTTP, Method: http.MethodPost, URL: "http://example.test/reload", OnFailure: config.HookOnFailureKeepRunning},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.ApplyWithPending(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:app_rules", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}, []string{"stale-pending"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), reloads.Load(), "shared dedup key means only one HTTP call")
	assert.ElementsMatch(t, []string{"current-tick", "stale-pending"}, result.AttemptedHookNames)
	require.Len(t, result.HookOutcomes, 2)
	outcomes := map[string]string{}
	for _, o := range result.HookOutcomes {
		outcomes[o.Name] = o.Result
	}
	assert.Equal(t, "success", outcomes["current-tick"])
	assert.Equal(t, "skipped", outcomes["stale-pending"])
	assert.Empty(t, result.PendingHookNames)
}

func TestApplyManifestTriggeredHookFires(t *testing.T) {
	t.Parallel()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().GetUnitStatus(mock.Anything, "vm.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	hooks := []config.Hook{
		{
			Name:      "scrape-reload",
			Manifests: []string{"config/scrape.yml"},
			Unit:      "vm.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload",
			OnFailure: config.HookOnFailureKeepRunning,
		},
		{
			Name:      "rules-reload",
			Manifests: []string{"config/rules.yml"},
			Unit:      "vm.service",
			Action:    config.HookActionHTTP,
			Method:    http.MethodPost,
			URL:       "http://example.test/reload-rules",
			OnFailure: config.HookOnFailureKeepRunning,
		},
	}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/var/lib/picolet/manifests/config/scrape.yml", Category: "manifest", Action: reconciler.ActionUpdate, NewContent: "x", RelPath: "config/scrape.yml"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), reloads.Load(), "exactly one matching hook fired")
	assert.ElementsMatch(t, []string{"scrape-reload"}, result.AttemptedHookNames)
}

func TestSecretHookReloaderHonorsHealthDelayCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		onFailure     string
		shouldRestart bool
	}{
		{name: "restart fallback", onFailure: config.HookOnFailureRestart, shouldRestart: true},
		{name: "keep running", onFailure: config.HookOnFailureKeepRunning, shouldRestart: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys := appliermocks.NewMockSystemdManager(t)
			sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
			pod := appliermocks.NewMockPodmanClient(t)
			ctx, cancel := context.WithCancel(context.Background())
			client := testHTTPClient(func(_ *http.Request) int {
				cancel()
				return http.StatusOK
			})
			reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(time.Hour)

			shouldRestart, err := reloader.Run(ctx, config.Hook{
				Name:      "app-reload",
				Secrets:   []string{"app_config"},
				Unit:      "app.service",
				Action:    config.HookActionHTTP,
				Method:    http.MethodPost,
				URL:       "http://example.test/reload",
				HealthURL: "http://example.test/health",
				OnFailure: tt.onFailure,
			}, nil)
			require.Error(t, err)
			assert.Equal(t, tt.shouldRestart, shouldRestart)
		})
	}
}

func TestCategoryOrderIncludesFileNextToManifest(t *testing.T) {
	t.Parallel()
	order := applier.CategoryOrder()
	manifestIdx := slices.Index(order, config.CategoryManifest)
	fileIdx := slices.Index(order, config.CategoryFile)
	require.NotEqual(t, -1, manifestIdx, "manifest must be present")
	require.NotEqual(t, -1, fileIdx, "file must be present")
	assert.Equal(t, manifestIdx+1, fileIdx, "file must come immediately after manifest")
}

func TestApplyFiresFilesOnlyHook(t *testing.T) {
	t.Parallel()

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()

	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	hooks := []config.Hook{{
		Name:      "scrape-reload",
		Files:     []string{"config/scrape.yml"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/var/lib/picolet/files/config/scrape.yml", Category: "file", Action: reconciler.ActionUpdate, NewContent: "x", RelPath: "config/scrape.yml"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), reloads.Load(), "files-only hook must fire (regression: applier.go:391 early-exit)")
	assert.ElementsMatch(t, []string{"scrape-reload"}, result.AttemptedHookNames)
}

func TestApplyRelPathDeleteDoesNotFireHooks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		category   config.Category
		destPath   string
		relPath    string
		hook       config.Hook
		wantReload bool
	}{
		{
			name:     "manifest",
			category: config.CategoryManifest,
			destPath: "/var/lib/picolet/manifests/config/scrape.yml",
			relPath:  "config/scrape.yml",
			hook: config.Hook{
				Name:      "manifest-reload",
				Manifests: []string{"config/scrape.yml"},
				Unit:      "app.service",
				Action:    config.HookActionHTTP,
				Method:    http.MethodPost,
				URL:       "http://example.test/reload",
				OnFailure: config.HookOnFailureKeepRunning,
			},
			wantReload: true,
		},
		{
			name:     "file",
			category: config.CategoryFile,
			destPath: "/var/lib/picolet/files/config/scrape.yml",
			relPath:  "config/scrape.yml",
			hook: config.Hook{
				Name:      "file-reload",
				Files:     []string{"config/scrape.yml"},
				Unit:      "app.service",
				Action:    config.HookActionHTTP,
				Method:    http.MethodPost,
				URL:       "http://example.test/reload",
				OnFailure: config.HookOnFailureKeepRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRelPathDeleteDoesNotFireHook(t, tt.category, tt.destPath, tt.relPath, tt.hook, tt.wantReload)
		})
	}
}

func assertRelPathDeleteDoesNotFireHook(
	t *testing.T,
	category config.Category,
	destPath, relPath string,
	hook config.Hook,
	wantReload bool,
) {
	t.Helper()
	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})
	sys := appliermocks.NewMockSystemdManager(t)
	if wantReload {
		sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	}
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, []config.Hook{hook}, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: destPath, Category: category, Action: reconciler.ActionDelete, RelPath: relPath},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(0), reloads.Load(), "rel-path delete hooks are intentionally not fired")
	assert.Empty(t, result.AttemptedHookNames)
	assert.Equal(t, []string{destPath}, fw.removed)
}
