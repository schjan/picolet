package picolet_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

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

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	require.NoError(t, validator.ValidateAll(t.Context(), r, cfg))
}

func TestIntegrationResolveGolden(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures"))

	for _, hostname := range cfg.SortedHostnames() {
		t.Run(hostname, func(t *testing.T) {
			t.Parallel()
			resolved, err := r.ResolveHost(t.Context(), hostname)
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

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	hostname := "node-1" // worker + app-a: exercises multi-feature assignment merging
	resolved, err := r.ResolveHost(t.Context(), hostname)
	require.NoError(t, err)

	// First deploy: all creates
	emptyState := state.NewState()
	cs := reconciler.Diff(resolved.Files, emptyState)
	assert.True(t, cs.HasChanges())
	assert.Equal(t, len(resolved.Files), cs.Summary[reconciler.ActionCreate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionUpdate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionDelete])

	// Build full state from changeset (simulating post-apply)
	fullState := state.NewState()
	for _, c := range cs.Changes {
		if c.Action != reconciler.ActionDelete {
			fullState.ManagedFiles[c.DestPath] = state.ManagedFile{Hash: c.NewHash, Category: c.Category}
			if c.ServiceName != "" {
				fullState.ServiceNames[c.DestPath] = c.ServiceName
			}
		}
	}

	// Idempotent: all noops
	cs2 := reconciler.Diff(resolved.Files, fullState)
	assert.False(t, cs2.HasChanges())
	assert.Equal(t, len(resolved.Files), cs2.Summary[reconciler.ActionNoop])
}

func TestIntegrationMultiHostConsistency(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	allResolved, err := r.ResolveAll(t.Context())
	require.NoError(t, err)

	// Base resources (network, systemd) should be identical across hosts
	node1 := filesByDest(allResolved["node-1"].Files)
	node2 := filesByDest(allResolved["node-2"].Files)

	// Both should have internal.network with identical content
	n1Net, ok1 := node1["/etc/containers/systemd/picolet/internal.network"]
	n2Net, ok2 := node2["/etc/containers/systemd/picolet/internal.network"]
	require.True(t, ok1, "node-1 should have internal.network")
	require.True(t, ok2, "node-2 should have internal.network")
	assert.Equal(t, n1Net.Content, n2Net.Content)

	// node-1 (worker + app-a) should have nginx.container
	assert.Contains(t, node1, "/etc/containers/systemd/picolet/nginx.container")
	// node-2 (controller) should NOT have nginx.container
	assert.NotContains(t, node2, "/etc/containers/systemd/picolet/nginx.container")

	// node-2 (controller) should have kube and manifest
	assert.Contains(t, node2, "/etc/containers/systemd/picolet/app-stack.kube")
	assert.Contains(t, node2, "/var/lib/picolet/manifests/app/deployment.yml")
	// node-1 (worker) should NOT have these
	assert.NotContains(t, node1, "/etc/containers/systemd/picolet/app-stack.kube")
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
		r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
		require.NoError(t, err)
		_, err = r.ResolveHost(t.Context(), "nonexistent")
		require.Error(t, err)
		var notFound *resolver.HostNotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, "nonexistent", notFound.Hostname)
	})
}

func newAggregatedSecretFleetFS(ruleExpr string) fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
pi_type: node
features: []
`)},
		"secrets/alerts.yml.tmpl": &fstest.MapFile{Data: []byte(`groups:{{ concatFiles "rules/*.yml" | nindent 2 -}}`)},
		"rules/instance.yml": &fstest.MapFile{Data: []byte(`- name: instance_alerts
  rules:
    - alert: InstanceDown
      expr: ` + ruleExpr + `
      for: 5m
`)},
		"rules/node.yml": &fstest.MapFile{Data: []byte(`- name: node_alerts
  rules:
    - alert: NodeUptimeLow
      expr: node_time_seconds - node_boot_time_seconds < 600
      for: 2m
`)},
	}
}

func TestIntegrationAggregatedSecretFragmentChangeTriggersUpdate(t *testing.T) {
	t.Parallel()

	fsysV1 := newAggregatedSecretFleetFS("up == 0")
	cfgV1, err := config.LoadAll(fsysV1)
	require.NoError(t, err)
	rV1, err := resolver.New(resolver.Config{FS: fsysV1, Config: cfgV1})
	require.NoError(t, err)
	resolvedV1, err := rV1.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolvedV1.Files, false))

	initialState := state.NewState()
	csV1 := reconciler.Diff(resolvedV1.Files, initialState)
	require.Equal(t, 1, csV1.Summary[reconciler.ActionCreate])
	for _, c := range csV1.Changes {
		if c.Action == reconciler.ActionDelete {
			continue
		}
		initialState.ManagedFiles[c.DestPath] = state.ManagedFile{Hash: c.NewHash, Category: c.Category}
	}

	fsysV2 := newAggregatedSecretFleetFS("up == 1")
	cfgV2, err := config.LoadAll(fsysV2)
	require.NoError(t, err)
	rV2, err := resolver.New(resolver.Config{FS: fsysV2, Config: cfgV2})
	require.NoError(t, err)
	resolvedV2, err := rV2.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolvedV2.Files, false))

	v1Secret := filesByDest(resolvedV1.Files)["secret:alerts"]
	v2Secret := filesByDest(resolvedV2.Files)["secret:alerts"]
	assert.NotEqual(t, v1Secret.Content, v2Secret.Content, "aggregated secret content should change when one fragment changes")

	csV2 := reconciler.Diff(resolvedV2.Files, initialState)
	assert.Equal(t, 1, csV2.Summary[reconciler.ActionUpdate], "changed aggregated secret should produce update")
}

func TestIntegrationAggregatedSecretMalformedFragmentFailsValidation(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
pi_type: node
features: []
`)},
		"secrets/alerts.yml.tmpl": &fstest.MapFile{Data: []byte(`groups:{{ concatFiles "rules/*.yml" | nindent 2 -}}`)},
		"rules/instance.yml":      &fstest.MapFile{Data: []byte("- name: instance_alerts\n  rules:\n    - alert: InstanceDown\n      expr: up == 0\n")},
		"rules/invalid.yml":       &fstest.MapFile{Data: []byte("- name: invalid\n  rules:\n    - alert: Broken\n      expr: [this is broken\n")},
	}
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	err = validator.ValidateFiles(resolved.Files, false)
	require.Error(t, err)
	require.ErrorContains(t, err, "secret:alerts")
	require.ErrorContains(t, err, "YAML parse error")
}
