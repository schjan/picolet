package agent

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

// A containerized Agent writes data files below its data dir while the
// .build names them by the host_data_dir path: editing the Containerfile
// rebuilds on the next reconciliation, and an unchanged one does not.
func TestReconcileRebuildsOnContainerfileChangeAtHostDataDir(t *testing.T) {
	t.Parallel()
	repoDir, dataDir, quadletDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
	writeTestFile(t, repoDir, "assignments.yml", "base:\n  paths: [quadlets, files]\n")
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", "hostname: test-host\nrole: server\nfeatures: []\n")
	writeTestFile(t, repoDir, "quadlets/app.build.tmpl",
		"[Build]\nImageTag=localhost/app\nFile={{ filePath \"app/Containerfile\" }}\nSetWorkingDirectory=file\n")
	writeTestFile(t, repoDir, "quadlets/app.container", "[Container]\nImage=app.build\n")
	writeTestFile(t, repoDir, "files/app/Containerfile", "FROM alpine:1\n")

	sys, pod, fw := newBareMocks(t)
	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir(), HostDataDir: "/srv/picolet"}
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw), WithRepoPath(repoDir),
		WithDataDir(dataDir), WithQuadletDir(quadletDir), WithSystemdDir(t.TempDir()))
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	containerfile := filepath.Join(dataDir, "files/app/Containerfile")

	// expectBuild expects one build of app-build.service and the restart of
	// its running consumer without dependencies.
	expectBuild := func() {
		pod.EXPECT().ImageID(mock.Anything, "localhost/app").Return("id", nil).Once()
		sys.EXPECT().RunBuildUnit(mock.Anything, "app-build.service").Return(nil).Once()
		sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active"}, nil).Once()
		sys.EXPECT().RestartUnitIgnoringDependencies(mock.Anything, "app.service").Return(nil).Once()
	}
	reconcile := func(sha string) {
		t.Helper()
		st, err := store.Load()
		require.NoError(t, err)
		_, err = a.ReconcileOnce(t.Context(), sha, st, store)
		require.NoError(t, err)
	}
	require.NoError(t, store.Save(state.NewState()))

	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(filepath.Join(quadletDir, "app.build"),
		[]byte("[Build]\nImageTag=localhost/app\nFile=/srv/picolet/files/app/Containerfile\nSetWorkingDirectory=file\n")).Return(nil).Once()
	fw.EXPECT().WriteFile(filepath.Join(quadletDir, "app.container"), []byte("[Container]\nImage=app.build\n")).Return(nil).Once()
	fw.EXPECT().WriteFile(containerfile, []byte("FROM alpine:1\n")).Return(nil).Once()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Once()
	expectBuild()
	reconcile("sha-1")

	// A data file changes: no daemon-reload, but a rebuild.
	writeTestFile(t, repoDir, "files/app/Containerfile", "FROM alpine:2\n")
	fw.EXPECT().WriteFile(containerfile, []byte("FROM alpine:2\n")).Return(nil).Once()
	expectBuild()
	reconcile("sha-2")

	// Nothing changed: the strict mocks fail on any write or build.
	reconcile("sha-3")
}
