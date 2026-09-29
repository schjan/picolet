package picolet_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
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

	// Idempotent: all noops
	cs2 := reconciler.Diff(resolved.Files, stateAfter(cs))
	assert.False(t, cs2.HasChanges())
	assert.Equal(t, len(resolved.Files), cs2.Summary[reconciler.ActionNoop])
}

// stateAfter builds the state a successful apply of cs leaves behind.
func stateAfter(cs *reconciler.Changeset) *state.State {
	st := state.NewState()
	for _, c := range cs.Changes {
		if c.Action != reconciler.ActionDelete {
			st.ManagedFiles[c.DestPath] = state.ManagedFile{Hash: c.NewHash, Category: c.Category}
			if c.ServiceName != "" {
				st.ServiceNames[c.DestPath] = c.ServiceName
			}
		}
	}
	return st
}

const (
	shopPodPath   = "/etc/containers/systemd/picolet/shop.pod"
	shopAPIPath   = "/etc/containers/systemd/picolet/shop-api.container"
	shopProxyPath = "/etc/containers/systemd/picolet/shop-proxy.container"
)

// TestIntegrationReconcilePipelinePod drives the example fleet's pod stack
// (shop.pod.tmpl + two member containers on node-1) through create, update
// and delete: changing the pod and a member restarts only the pod service,
// and removing the stack stops the pod's generated <name>-pod.service.
func TestIntegrationReconcilePipelinePod(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "node-1")
	require.NoError(t, err)

	created := reconciler.Diff(resolved.Files, state.NewState())
	pod := findChange(t, created, shopPodPath)
	assert.Equal(t, reconciler.ActionCreate, pod.Action)
	assert.Equal(t, config.CategoryPod, pod.Category)
	assert.Equal(t, "shop-pod.service", pod.ServiceName)
	deployed := stateAfter(created)

	// Update the pod and one member: only the pod service restarts.
	updated := slices.Clone(resolved.Files)
	for i := range updated {
		if updated[i].DestPath == shopPodPath || updated[i].DestPath == shopAPIPath {
			updated[i].Content += "# rev 2\n"
		}
	}
	cs := reconciler.Diff(updated, deployed)
	require.Equal(t, 2, cs.Summary[reconciler.ActionUpdate])
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "shop-pod.service").Return(nil).Once()
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(shopPodPath, mock.Anything).Return(nil).Once()
	fw.EXPECT().WriteFile(shopAPIPath, mock.Anything).Return(nil).Once()
	result, err := applier.New(sys, appliermocks.NewMockPodmanClient(t), fw, false, nil).Apply(t.Context(), cs)
	require.NoError(t, err)
	assert.Equal(t, []string{"shop-pod.service"}, result.RestartedUnits)

	applyDeletes(t, resolved.Files, deployed, map[string]string{
		shopPodPath:   "shop-pod.service",
		shopAPIPath:   "shop-api.service",
		shopProxyPath: "shop-proxy.service",
	})
}

// applyDeletes removes the files keyed in units (DestPath → generated service)
// from the desired files, checks that what remains still validates, and
// applies the resulting deletes: each service, named from state, is stopped
// before its file is removed.
func applyDeletes(t *testing.T, files []resolver.ResolvedFile, deployed *state.State, units map[string]string) {
	t.Helper()
	remaining := slices.DeleteFunc(slices.Clone(files), func(f resolver.ResolvedFile) bool {
		_, ok := units[f.DestPath]
		return ok
	})
	require.NoError(t, validator.ValidateFiles(remaining, false))
	cs := reconciler.Diff(remaining, deployed)
	require.Equal(t, len(units), cs.Summary[reconciler.ActionDelete])
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	fw := appliermocks.NewMockFileWriter(t)
	for path, unit := range units {
		require.Equal(t, unit, findChange(t, cs, path).ServiceName)
		stop := sys.EXPECT().StopUnit(mock.Anything, unit).Return(nil).Once()
		fw.EXPECT().Remove(path).Return(nil).Once().NotBefore(stop)
	}
	_, err := applier.New(sys, appliermocks.NewMockPodmanClient(t), fw, false, nil).Apply(t.Context(), cs)
	require.NoError(t, err)
}

func findChange(t *testing.T, cs *reconciler.Changeset, destPath string) reconciler.Change {
	t.Helper()
	i := slices.IndexFunc(cs.Changes, func(c reconciler.Change) bool { return c.DestPath == destPath })
	require.GreaterOrEqual(t, i, 0, "no change for %s", destPath)
	return cs.Changes[i]
}

const (
	shopAPIBuildPath = "/etc/containers/systemd/picolet/shop-api.build"
	nginxImagePath   = "/etc/containers/systemd/picolet/nginx.image"
)

// TestIntegrationReconcilePipelineBuildAndImage drives node-1's .build and
// .image through create and delete: each is tracked under its generated
// <name>-build.service / <name>-image.service, the build's File= names the
// Containerfile deployed from files/, and removing them together with their
// consumers stops those services before their files are removed.
func TestIntegrationReconcilePipelineBuildAndImage(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "node-1")
	require.NoError(t, err)

	created := reconciler.Diff(resolved.Files, state.NewState())
	build := findChange(t, created, shopAPIBuildPath)
	assert.Equal(t, config.CategoryBuild, build.Category)
	assert.Equal(t, "shop-api-build.service", build.ServiceName)
	image := findChange(t, created, nginxImagePath)
	assert.Equal(t, config.CategoryImage, image.Category)
	assert.Equal(t, "nginx-image.service", image.ServiceName)
	i := slices.IndexFunc(created.Changes, func(c reconciler.Change) bool {
		return c.Category == config.CategoryFile && c.RelPath == "shop-api/Containerfile"
	})
	require.GreaterOrEqual(t, i, 0, "Containerfile not deployed")
	assert.Contains(t, build.NewContent, "File="+created.Changes[i].DestPath+"\n")

	// The consumers go too: a container naming an absent build/image fails validation.
	applyDeletes(t, resolved.Files, stateAfter(created), map[string]string{
		shopAPIBuildPath: "shop-api-build.service",
		nginxImagePath:   "nginx-image.service",
		shopAPIPath:      "shop-api.service",
		shopProxyPath:    "shop-proxy.service",
	})
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

	pathsCases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "manifests below the first segment",
			files: map[string]string{"app/manifests/deploy.yml": "kind: ConfigMap\n"},
			want:  `app/manifests/deploy.yml: "manifests/" must be the first path segment`,
		},
		{
			name:  "manifests and files in one path",
			files: map[string]string{"app/manifests/files/deploy.yml": "kind: ConfigMap\n"},
			want:  `app/manifests/files/deploy.yml: path has both "manifests/" and "files/"`,
		},
		{
			name:  "unknown extension",
			files: map[string]string{"app/web.contaner": "[Container]\n"},
			want:  `app/web.contaner: unknown extension ".contaner"; move it under files/ or remove it from the listed directory`,
		},
		{
			name:  "extensionless file",
			files: map[string]string{"app/Containerfile": "FROM scratch\n"},
			want:  "app/Containerfile: no file extension; move it under files/ or remove it from the listed directory",
		},
		{
			name:  "destination collision",
			files: map[string]string{"app/web.network": "[Network]\n", "app/nested/web.network": "[Network]\n"},
			want:  "destination collision for /etc/containers/systemd/picolet/web.network: app/nested/web.network, app/web.network",
		},
	}
	for _, tc := range pathsCases {
		t.Run("paths: "+tc.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, validatePathsFleet(t, tc.files), tc.want)
		})
	}
}

// validatePathsFleet runs `picolet validate` on a one-host fleet whose base
// group lists `paths: [app/]`; files maps Fleet paths to content.
func validatePathsFleet(t *testing.T, files map[string]string) error {
	t.Helper()
	repoFS := fstest.MapFS{
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml":          &fstest.MapFile{Data: []byte("base:\n  paths: [app/]\nroles: {}\nfeatures: {}\n")},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte("hostname: test-host\nexternal_hostname: test-host.ts.net\nrole: node\nfeatures: []\n")},
	}
	for p, content := range files {
		repoFS[p] = &fstest.MapFile{Data: []byte(content)}
	}
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	return validator.ValidateAll(t.Context(), r, cfg)
}

func newAggregatedSecretFleetFS(ruleExpr string) fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
roles: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
role: node
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

func newSystemdUnitsFleetFS() fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images:\n  node_exporter: \"prom/node-exporter:v1.8\"\n  web: \"nginx:1.27\"\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  networks:
    - quadlets/internal.network
  systemd:
    - systemd/health.timer
  containers:
    - quadlets/web.container
    - quadlets/node-exporter.container.tmpl
roles: {}
features: {}
`)},
		"hosts/mon-host/host.yml":   &fstest.MapFile{Data: []byte("hostname: mon-host\nexternal_hostname: mon-host.ts.net\nrole: node\nfeatures: []\n")},
		"quadlets/internal.network": &fstest.MapFile{Data: []byte("[Network]\nInternal=true\n")},
		"quadlets/web.container":    &fstest.MapFile{Data: []byte("[Container]\nImage=nginx:1.27\nContainerName=web\n\n[Install]\nWantedBy=default.target\n")},
		"systemd/health.timer":      &fstest.MapFile{Data: []byte("[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n")},
		"quadlets/node-exporter.container.tmpl": &fstest.MapFile{Data: []byte(`[Unit]
Description=Prometheus Node Exporter

[Container]
Image={{ index .Images "node_exporter" }}
ContainerName=node-exporter
Exec=--collector.systemd.unit-include=^({{ range $i, $u := .Host.SystemdUnits }}{{ if $i }}|{{ end }}{{ trimSuffix ".service" $u }}{{ end }})$

[Install]
WantedBy=default.target
`)},
	}
}

// TestIntegrationSystemdUnitsTemplate exercises a node-exporter-style template
// that builds its unit-include regex from .Host.SystemdUnits — including the
// node-exporter unit itself (self-reference).
func TestIntegrationSystemdUnitsTemplate(t *testing.T) {
	t.Parallel()
	fsys := newSystemdUnitsFleetFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "mon-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolved.Files, false))

	exporter := filesByDest(resolved.Files)["/etc/containers/systemd/picolet/node-exporter.container"]
	require.NotEmpty(t, exporter.Content, "node-exporter.container should be resolved")
	// .Host.SystemdUnits covers the container, network, raw-systemd and the
	// node-exporter unit itself, sorted and deduplicated.
	assert.Contains(t, exporter.Content,
		"unit-include=^(health.timer|internal-network|node-exporter|web)$")

	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures"))
	g.Assert(t, "systemd-units/node-exporter.container", []byte(exporter.Content))
}

func TestIntegrationAggregatedSecretMalformedFragmentFailsValidation(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
roles: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
role: node
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
