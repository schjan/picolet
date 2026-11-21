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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
)

// TestE2EImagePrune exercises the production SocketPodmanClient.ImagePrune(all=true)
// path against a real Podman socket.
//
// `image prune -a` removes every image no container on the socket uses, so it
// runs only against its scenario's own disposable store, which no other
// scenario shares. The assertions are made deterministic by checking specific
// image IDs rather than reclaimed byte counts: a uniquely-built image with no
// container must be removed, while an image referenced by a running container
// must survive.
//
//nolint:paralleltest // one scenario per process
func TestE2EImagePrune(t *testing.T) {
	rt := isolatedRuntime(t)
	socketPath := rt.PodmanSocket
	ctx := context.Background()

	const keepRef = "docker.io/library/alpine:3.23"
	requireImage(t, keepRef)
	keepID := pruneImageID(t, keepRef)
	require.NotEmpty(t, keepID, "keep image must resolve to an ID")

	const keepName = "picolet-e2e-prune-keep"
	pruneRunPodman(t, "run", "-d", "--name", keepName, keepRef, "sleep", "600")

	// A uniquely-tagged image not referenced by any container — the prune target.
	unusedTag := fmt.Sprintf("localhost/picolet-e2e-prune-unused:%d", time.Now().UnixNano())
	pruneBuildUniqueImage(t, unusedTag, keepRef)
	unusedID := pruneImageID(t, unusedTag)
	require.NotEmpty(t, unusedID, "unused image must resolve to an ID")
	require.NotEqual(t, keepID, unusedID, "unused image must be distinct from the kept image")

	podman, err := applier.NewSocketPodmanClient(ctx, socketPath)
	require.NoError(t, err)

	// Never assert ReclaimedBytes > 0: other images may already be pruned, making
	// the exact figure non-deterministic. The specific-ID assertions below are the
	// deterministic checks.
	_, err = podman.ImagePrune(ctx, true)
	require.NoError(t, err)

	assert.False(t, pruneImageExists(t, unusedID), "unused image should be removed by prune -a")
	assert.True(t, pruneImageExists(t, keepID), "image referenced by a container must survive prune -a")
}

func pruneRunPodman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("podman", args...).CombinedOutput() //nolint:gosec // test-controlled args
	require.NoError(t, err, "podman %s: %s", strings.Join(args, " "), out)
	return string(out)
}

func pruneImageID(t *testing.T, ref string) string {
	t.Helper()
	return strings.TrimSpace(pruneRunPodman(t, "image", "inspect", "--format", "{{.Id}}", ref))
}

func pruneImageExists(t *testing.T, id string) bool {
	t.Helper()
	return exec.Command("podman", "image", "exists", id).Run() == nil //nolint:gosec // test-controlled args
}

// pruneBuildUniqueImage builds a small image with a unique layer (so its ID is
// distinct) on top of base, leaving it referenced by no container.
func pruneBuildUniqueImage(t *testing.T, tag, base string) {
	t.Helper()
	dir := t.TempDir()
	cf := filepath.Join(dir, "Containerfile")
	content := fmt.Sprintf("FROM %s\nRUN echo %s > /picolet-e2e-prune-marker\n", base, tag)
	require.NoError(t, os.WriteFile(cf, []byte(content), 0o600))
	pruneRunPodman(t, "build", "-t", tag, "-f", cf, dir)
}
