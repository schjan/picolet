package applier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/reconciler"
)

// testSystemdManager is a minimal mock for SystemdManager defined in-package to
// avoid the circular import that arises when importing mocks/applier (which imports
// pkg/applier for UnitStatus).
type testSystemdManager struct {
	mock.Mock
}

func newTestSystemd(t *testing.T) *testSystemdManager {
	t.Helper()
	s := &testSystemdManager{}
	s.Test(t)
	t.Cleanup(func() { s.AssertExpectations(t) })
	return s
}

func (m *testSystemdManager) DaemonReload(ctx context.Context) error {
	return m.Called(ctx).Error(0)
}

func (m *testSystemdManager) StartUnit(ctx context.Context, name string) error {
	return m.Called(ctx, name).Error(0)
}

func (m *testSystemdManager) StopUnit(ctx context.Context, name string) error {
	return m.Called(ctx, name).Error(0)
}

func (m *testSystemdManager) RestartUnit(ctx context.Context, name string) error {
	return m.Called(ctx, name).Error(0)
}

func (m *testSystemdManager) GetUnitStatus(ctx context.Context, name string) (UnitStatus, error) {
	args := m.Called(ctx, name)
	us, _ := args.Get(0).(UnitStatus)
	return us, args.Error(1)
}

func TestApplyPhaseOrdering(t *testing.T) {
	t.Parallel()
	sys := newTestSystemd(t)
	sys.On("DaemonReload", mock.Anything).Return(nil)
	sys.On("RestartUnit", mock.Anything, mock.Anything).Return(nil).Maybe()

	pod := newMockPodman(t)
	pod.On("SecretCreate", mock.Anything, "my_secret", []byte("token=abc"), false).Return(nil)

	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

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
	sys := newTestSystemd(t)
	pod := newMockPodman(t)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, true)

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
	sys := newTestSystemd(t)
	pod := newMockPodman(t)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

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
	sys := newTestSystemd(t)
	sys.On("StopUnit", mock.Anything, "old.service").Return(nil)
	sys.On("DaemonReload", mock.Anything).Return(nil)
	// RestartUnit must NOT be called for deletes — the unit is gone after daemon-reload
	pod := newMockPodman(t)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

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
	sys := newTestSystemd(t)
	pod := newMockPodman(t)
	pod.On("SecretRemove", mock.Anything, "old_secret").Return(nil)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:old_secret", Category: "secret", Action: reconciler.ActionDelete},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
	// Secret deletes go through podman, not file writer
	assert.Empty(t, fw.removed)
}

func TestApplySelfRestart(t *testing.T) {
	t.Parallel()
	sys := newTestSystemd(t)
	sys.On("DaemonReload", mock.Anything).Return(nil)
	// goroutine fires asynchronously after Apply() returns; may or may not complete before test cleanup
	sys.On("RestartUnit", mock.Anything, "picolet.service").Return(nil).Maybe()
	pod := newMockPodman(t)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

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
	sys := newTestSystemd(t)
	pod := newMockPodman(t)
	// ActionUpdate → replace=true
	pod.On("SecretCreate", mock.Anything, "cfg", []byte("new-data"), true).Return(nil)
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

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

// mockPodman is a minimal PodmanClient mock for applier tests.
type mockPodman struct {
	mock.Mock
}

func newMockPodman(t *testing.T) *mockPodman {
	t.Helper()
	p := &mockPodman{}
	p.Test(t)
	t.Cleanup(func() { p.AssertExpectations(t) })
	return p
}

func (m *mockPodman) SecretExists(ctx context.Context, name string) (bool, error) {
	args := m.Called(ctx, name)
	return args.Bool(0), args.Error(1)
}

func (m *mockPodman) SecretCreate(ctx context.Context, name string, data []byte, replace bool) error {
	return m.Called(ctx, name, data, replace).Error(0)
}

func (m *mockPodman) SecretRemove(ctx context.Context, name string) error {
	return m.Called(ctx, name).Error(0)
}

func (m *mockPodman) ListManagedSecrets(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	ss, _ := args.Get(0).([]string)
	return ss, args.Error(1)
}

func (m *mockPodman) ContainerRemove(ctx context.Context, nameOrID string, force bool) error {
	return m.Called(ctx, nameOrID, force).Error(0)
}

func (m *mockPodman) RunHealthcheck(ctx context.Context, container string) (bool, error) {
	args := m.Called(ctx, container)
	return args.Bool(0), args.Error(1)
}

func (m *mockPodman) GetPodState(ctx context.Context, pod string) (string, error) {
	args := m.Called(ctx, pod)
	return args.String(0), args.Error(1)
}
