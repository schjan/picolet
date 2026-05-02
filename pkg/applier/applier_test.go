package applier_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
)

func TestApplyPhaseOrdering(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretCreate(mock.Anything, "my_secret", []byte("token=abc"), false).Return(nil)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false)

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
	a := applier.New(sys, pod, fw, true)

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
	a := applier.New(sys, pod, fw, false)

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
	a := applier.New(sys, pod, fw, false)

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
	sys := appliermocks.NewMockSystemdManager(t)
	pod := appliermocks.NewMockPodmanClient(t)
	pod.EXPECT().SecretRemove(mock.Anything, "old_secret").Return(nil)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false)

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
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	// goroutine fires asynchronously after Apply() returns; may or may not complete before test cleanup
	sys.EXPECT().RestartUnit(mock.Anything, "picolet.service").Return(nil).Maybe()
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()
	a := applier.New(sys, pod, fw, false)

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
	a := applier.New(sys, pod, fw, false)

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
