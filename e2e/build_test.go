//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/cli"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/state"
)

const (
	e2eBuildBase      = "docker.io/library/alpine:3.23"
	e2eBuildImage     = "localhost/picolet-e2e-build:latest"
	e2eBuildContainer = "picolet-e2e-build"
	e2eBuildService   = "e2e-build-build.service"
	e2eBuildLabel     = "io.picolet.e2e"
	e2eBuildContext   = "delivered build context\n"
)

// setupBuildFleet creates a fleet whose container runs an image built by a
// .build unit from a Containerfile delivered under files/, whose build
// context (the Containerfile's directory) holds a second delivered file.
func setupBuildFleet(t *testing.T, fleetDir string) {
	t.Helper()
	files := map[string]string{
		"fleet.yml": "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml": `base: {}
roles:
  build:
    paths:
      - quadlets/builds/e2e-build.build.tmpl
      - quadlets/containers/e2e-build.container
      - files/e2e-build
      - files/e2e-other/notes.txt
`,
		"hosts/build-host/host.yml": "hostname: build-host\nrole: build\nfeatures: []\n",
		"quadlets/builds/e2e-build.build.tmpl": `[Build]
ImageTag=` + e2eBuildImage + `
File={{ filePath "e2e-build/Containerfile" }}
SetWorkingDirectory=file
`,
		"quadlets/containers/e2e-build.container": `[Container]
Image=e2e-build.build
ContainerName=` + e2eBuildContainer + `

[Install]
WantedBy=default.target
`,
		"files/e2e-build/Containerfile": buildContainerfile("build", "COPY context.txt /context.txt\n"),
		"files/e2e-build/context.txt":   e2eBuildContext,
		"files/e2e-other/notes.txt":     "unrelated to the build\n",
	}
	for name, content := range files {
		path := filepath.Join(fleetDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

// buildContainerfile returns the e2e Containerfile, labelled label, with an
// optional extra instruction (e.g. a failing RUN).
func buildContainerfile(label, extra string) string {
	return "FROM " + e2eBuildBase + "\n" + extra + "LABEL " + e2eBuildLabel + "=" + label + "\nCMD [\"sleep\", \"infinity\"]\n"
}

// removeBuildArtifacts removes the test container and the built image.
func removeBuildArtifacts() {
	_ = exec.Command("podman", "rm", "-f", e2eBuildContainer).Run()
	_ = exec.Command("podman", "rmi", "-f", e2eBuildImage).Run()
}

// TestE2EBuild is the .build happy path against real systemd and Podman:
// `picolet apply` deploys a .build, its Containerfile and a container with
// Image=<name>.build; starting the container runs the generated build service,
// the container runs the built image, and `picolet down` removes it again.
//
// Runs serially (no t.Parallel): runCLI mutates slog's default logger, and a
// concurrent `image prune -a` (TestE2EImagePrune) could remove the image mid-build.
//
//nolint:paralleltest,funlen,cyclop // serial for the reasons above; sequential sub-tests sharing helper closures
func TestE2EBuild(t *testing.T) {
	requirePodmanAtLeast(t, 5, 2) // Quadlet .build support landed in Podman 5.2.0
	socketPath := podmanSocketPath(t)
	dataDir := t.TempDir()
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	setupBuildFleet(t, fleetDir)

	configPath := filepath.Join(dataDir, "config.yml")
	cfg := fmt.Sprintf("hostname: build-host\nrootless: true\ndata_dir: %s\nsecrets_dir: %s\npodman_socket: %s\n",
		dataDir, t.TempDir(), socketPath)
	require.NoError(t, os.WriteFile(configPath, []byte(cfg), 0o600))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	quadletDir := filepath.Join(home, ".config", "containers", "systemd", "picolet")

	removeBuildArtifacts()
	// The container restart waits for the build inside systemd's job (picolet
	// gives a job 30s); pulling the base image up front keeps a slow registry
	// out of that budget.
	out, err := exec.Command("podman", "pull", e2eBuildBase).CombinedOutput()
	require.NoError(t, err, "podman pull %s: %s", e2eBuildBase, out)
	t.Cleanup(func() {
		// Best-effort; t.Context is already cancelled at cleanup.
		_ = cli.Execute(context.Background(), []string{"picolet", "down", "--config", configPath})
		removeBuildArtifacts()
	})

	// Everything below depends on the apply; stop instead of cascading timeouts.
	// The consumer's unit state is checked too, so a failure shows the
	// diagnostics here rather than in a later subtest.
	require.True(t, t.Run("apply", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		require.NoError(t, runCLI(t, "apply", "--host", "build-host", "--repo-dir", fleetDir, "--config", configPath))
		systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
		require.NoError(t, err)
		defer systemd.Close()
		st, err := systemd.GetUnitStatus(t.Context(), "e2e-build.service")
		require.NoError(t, err)
		require.NotEqual(t, "failed", st.ActiveState, "e2e-build.service failed to start (see diagnostics)")
	}), "apply failed")

	t.Run("container_runs_built_image", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
			return err == nil && data.State.Status == "running"
		}, 60*time.Second, 2*time.Second, "container %s should reach running state", e2eBuildContainer)

		data, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
		require.NoError(t, err)
		assert.Equal(t, e2eBuildImage, data.ImageName)
		img, err := images.GetImage(connCtx, e2eBuildImage, nil)
		require.NoError(t, err)
		assert.Equal(t, "build", img.Labels[e2eBuildLabel], "image must come from the delivered Containerfile")
		out, err := exec.Command("podman", "exec", e2eBuildContainer, "cat", "/context.txt").CombinedOutput()
		require.NoError(t, err, "podman exec: %s", out)
		assert.Equal(t, e2eBuildContext, string(out), "the build context must be the delivered files/ directory")
	})

	t.Run("build_service_finished", func(t *testing.T) {
		systemd, err := applier.NewDBusSystemdManager(t.Context(), true)
		require.NoError(t, err)
		defer systemd.Close()
		st, err := systemd.GetUnitStatus(t.Context(), e2eBuildService)
		require.NoError(t, err)
		// Podman sets no RemainAfterExit for builds: a successful build is inactive.
		assert.Equal(t, "inactive", st.ActiveState)
	})

	t.Run("state_tracks_build", func(t *testing.T) {
		st, err := state.NewStore(filepath.Join(dataDir, "state.json")).Load()
		require.NoError(t, err)
		assert.Equal(t, e2eBuildService, st.ServiceNames[filepath.Join(quadletDir, "e2e-build.build")])
		// Managed files go to the rootless default data dir, not config's
		// data_dir (picolet's runtime dir): match by suffix.
		found := false
		for path, mf := range st.ManagedFiles {
			if strings.HasSuffix(path, "/files/e2e-build/Containerfile") && mf.Category == config.CategoryFile {
				found = true
			}
		}
		assert.True(t, found, "the Containerfile is a managed file: %v", st.ManagedFiles)
	})

	connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
	require.NoError(t, err)
	// consumer returns when the consumer container started and its image ID.
	consumer := func(t *testing.T) (time.Time, string) {
		t.Helper()
		data, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
		require.NoError(t, err)
		require.Equal(t, "running", data.State.Status)
		return data.State.StartedAt, data.Image
	}
	// buildRun identifies the build service's last run.
	buildRun := func(t *testing.T) string {
		t.Helper()
		out, err := exec.Command("systemctl", "--user", "show", "-p", "InvocationID", "--value", e2eBuildService).Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	imageLabel := func(t *testing.T) string {
		t.Helper()
		img, err := images.GetImage(connCtx, e2eBuildImage, nil)
		require.NoError(t, err)
		return img.Labels[e2eBuildLabel]
	}
	writeFleetAndApply := func(t *testing.T, rel, content string) error {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, rel), []byte(content), 0o600))
		return runCLI(t, "apply", "--host", "build-host", "--repo-dir", fleetDir, "--config", configPath)
	}

	t.Run("unrelated_change_rebuilds_nothing", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		started, image := consumer(t)
		run := buildRun(t)
		require.NoError(t, writeFleetAndApply(t, "files/e2e-other/notes.txt", "changed\n"))
		gotStarted, gotImage := consumer(t)
		assert.Equal(t, started, gotStarted, "consumer must not restart")
		assert.Equal(t, image, gotImage)
		assert.Equal(t, run, buildRun(t), "build must not run")
	})

	t.Run("containerfile_change_rebuilds_and_restarts_consumer", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		started, image := consumer(t)
		run := buildRun(t)
		require.NoError(t, writeFleetAndApply(t, "files/e2e-build/Containerfile", buildContainerfile("rebuilt", "")))
		assert.NotEqual(t, run, buildRun(t), "build must run")
		assert.Equal(t, "rebuilt", imageLabel(t))
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
			return err == nil && data.State.Status == "running" && data.Image != image && data.State.StartedAt.After(started)
		}, 60*time.Second, 2*time.Second, "consumer should run the rebuilt image")
	})

	t.Run("failing_build_leaves_consumer_running", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		started, image := consumer(t)
		require.Error(t, writeFleetAndApply(t, "files/e2e-build/Containerfile", buildContainerfile("broken", "RUN false\n")))
		assert.Equal(t, "rebuilt", imageLabel(t), "the previously built image keeps its tag")
		gotStarted, gotImage := consumer(t)
		assert.Equal(t, started, gotStarted, "consumer must not restart")
		assert.Equal(t, image, gotImage)
		st, err := state.NewStore(filepath.Join(dataDir, "state.json")).Load()
		require.NoError(t, err)
		found := false
		for path := range st.ManagedFiles {
			if strings.HasSuffix(path, "/files/e2e-build/Containerfile") {
				found = true
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, buildContainerfile("rebuilt", ""), string(content), "the deployed Containerfile is rolled back")
			}
		}
		assert.True(t, found, "the Containerfile is a managed file")
	})

	// A consumer down since a failed build is started by the fixing commit.
	t.Run("fixed_build_starts_stopped_consumer", func(t *testing.T) {
		defer dumpBuildDiagnostics(t)
		out, err := exec.Command("systemctl", "--user", "stop", "e2e-build.service").CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NoError(t, writeFleetAndApply(t, "files/e2e-build/Containerfile", buildContainerfile("fixed", "")))
		assert.Equal(t, "fixed", imageLabel(t))
		require.Eventually(t, func() bool {
			data, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
			return err == nil && data.State.Status == "running" && data.Config.Labels[e2eBuildLabel] == "fixed"
		}, 60*time.Second, 2*time.Second, "consumer should run the fixed image")
	})

	t.Run("down", func(t *testing.T) {
		require.NoError(t, runCLI(t, "down", "--config", configPath))
		connCtx, err := bindings.NewConnection(t.Context(), "unix:"+socketPath)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			_, err := containers.Inspect(connCtx, e2eBuildContainer, nil)
			return err != nil
		}, 30*time.Second, 2*time.Second, "container %s should be removed", e2eBuildContainer)
		assert.NoFileExists(t, filepath.Join(quadletDir, "e2e-build.build"))
		assert.NoFileExists(t, filepath.Join(quadletDir, "e2e-build.container"))
	})
}

// requirePodmanAtLeast fails the test when the host's Podman is older than
// major.minor, naming the version, instead of letting a missing Quadlet
// feature surface as a timeout.
func requirePodmanAtLeast(t *testing.T, major, minor int) {
	t.Helper()
	out, err := exec.Command("podman", "version", "--format", "{{.Client.Version}}").Output()
	require.NoError(t, err, "podman version")
	version := strings.TrimSpace(string(out))
	var gotMajor, gotMinor int
	_, err = fmt.Sscanf(version, "%d.%d", &gotMajor, &gotMinor)
	require.NoError(t, err, "parsing podman version %q", version)
	require.Truef(t, gotMajor > major || (gotMajor == major && gotMinor >= minor),
		"needs Podman >= %d.%d, host has %s", major, minor, version)
}

// dumpBuildDiagnostics logs the Podman version and the build and consumer
// units' status and journal when t has failed; call it deferred while the
// units still exist.
func dumpBuildDiagnostics(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	for _, args := range [][]string{
		{"podman", "--version"},
		{"systemctl", "--user", "status", "--no-pager", "e2e-build.service", e2eBuildService},
		{"journalctl", "--user", "--no-pager", "-n", "50", "-u", "e2e-build.service", "-u", e2eBuildService},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput() //nolint:gosec // fixed diagnostic commands
		t.Logf("$ %s (err: %v)\n%s", strings.Join(args, " "), err, out)
	}
}
