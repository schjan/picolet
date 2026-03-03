package picolet_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

const testdataDir = "testdata/example-fleet"

func TestIntegrationValidate(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r := resolver.New(repoFS, cfg, nil)
	v := validator.New()
	require.NoError(t, v.ValidateAll(context.Background(), r, cfg))
}

func TestIntegrationResolveGolden(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r := resolver.New(repoFS, cfg, nil)
	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures"))

	for _, hostname := range cfg.SortedHostnames() {
		t.Run(hostname, func(t *testing.T) {
			t.Parallel()
			resolved, err := r.ResolveHost(hostname)
			require.NoError(t, err)

			for _, f := range resolved.Files {
				name := hostname + "/" + sanitizePath(f.DestPath)
				g.Assert(t, name, []byte(f.Content))
			}
		})
	}
}

// sanitizePath converts a dest path to a safe golden file name.
func sanitizePath(destPath string) string {
	// "secret:foo" → "secret_foo", "/etc/..." → "etc_..."
	s := strings.ReplaceAll(destPath, ":", "_")
	s = strings.TrimPrefix(s, "/")
	return strings.ReplaceAll(s, "/", "_")
}

func TestIntegrationReconcilePipeline(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r := resolver.New(repoFS, cfg, nil)
	hostname := cfg.SortedHostnames()[0] // node-1 (has feature app-a)
	resolved, err := r.ResolveHost(hostname)
	require.NoError(t, err)

	rec := reconciler.New()

	// First deploy: all creates
	emptyState := &state.State{ManagedFiles: make(map[string]string)}
	cs := rec.Diff(resolved.Files, emptyState, nil)
	assert.True(t, cs.HasChanges())
	assert.Equal(t, len(resolved.Files), cs.Summary[reconciler.ActionCreate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionUpdate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionDelete])

	// Build full state from changeset (simulating post-apply)
	fullState := &state.State{ManagedFiles: make(map[string]string)}
	for _, c := range cs.Changes {
		if c.Action != reconciler.ActionDelete {
			fullState.ManagedFiles[c.DestPath] = c.NewHash
		}
	}

	// Idempotent: all noops
	cs2 := rec.Diff(resolved.Files, fullState, nil)
	assert.False(t, cs2.HasChanges())
	assert.Equal(t, len(resolved.Files), cs2.Summary[reconciler.ActionNoop])
}

func TestIntegrationMultiHostConsistency(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r := resolver.New(repoFS, cfg, nil)
	allResolved, err := r.ResolveAll()
	require.NoError(t, err)

	// Base resources (network, systemd, exporter container) should be identical across hosts
	node1 := filesByDest(allResolved["node-1"].Files)
	node2 := filesByDest(allResolved["node-2"].Files)

	// Both should have internal.network with identical content
	n1Net, ok1 := node1["/etc/containers/systemd/internal.network"]
	n2Net, ok2 := node2["/etc/containers/systemd/internal.network"]
	require.True(t, ok1, "node-1 should have internal.network")
	require.True(t, ok2, "node-2 should have internal.network")
	assert.Equal(t, n1Net.Content, n2Net.Content)

	// node-1 (worker + app-a) should have nginx.container
	assert.Contains(t, node1, "/etc/containers/systemd/nginx.container")
	// node-2 (controller) should NOT have nginx.container
	assert.NotContains(t, node2, "/etc/containers/systemd/nginx.container")

	// node-2 (controller) should have kube and manifest
	assert.Contains(t, node2, "/etc/containers/systemd/app-stack.kube")
	assert.Contains(t, node2, "/var/lib/picolet/manifests/app/deployment.yml")
	// node-1 (worker) should NOT have these
	assert.NotContains(t, node1, "/etc/containers/systemd/app-stack.kube")
	assert.NotContains(t, node1, "/var/lib/picolet/manifests/app/deployment.yml")
}

func filesByDest(files []resolver.ResolvedFile) map[string]resolver.ResolvedFile {
	m := make(map[string]resolver.ResolvedFile, len(files))
	for _, f := range files {
		m[f.DestPath] = f
	}
	return m
}

func TestIntegrationErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("unknown host", func(t *testing.T) {
		t.Parallel()
		repoFS := os.DirFS(testdataDir)
		cfg, err := config.LoadAll(repoFS)
		require.NoError(t, err)
		r := resolver.New(repoFS, cfg, nil)
		_, err = r.ResolveHost("nonexistent")
		require.Error(t, err)
		var notFound *resolver.HostNotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, "nonexistent", notFound.Hostname)
	})
}
