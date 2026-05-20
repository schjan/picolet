package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

//nolint:funlen // in-memory filesystem construction
func newTestFS() fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  traefik: "traefik:v3.6.9"
  alloy: "alloy:v1.13.2"
  tailscale: "tailscale:v1.94.2"
ports:
  alloy_http: 12345
  alloy_prometheus: 9090
  alloy_otlp_grpc: 4317
  alloy_otlp_http: 4318
  prometheus: 9090
prometheus:
  scrape_interval: "15s"
  scrape_timeout: "10s"
  exporter_scrape_interval: "30s"
  retention_time: "35d"
  retention_size: "2GB"
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  networks:
    - quadlets/networks/internal.network
  containers:
    - quadlets/containers/test.container.tmpl
  manifests:
    - manifests/app/deployment.yml.tmpl
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
		"quadlets/networks/internal.network": &fstest.MapFile{Data: []byte(`[Network]
Internal=true
`)},
		"quadlets/containers/test.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
ContainerName=test
Image={{index .Images "traefik"}}
Network=internal.network

[Install]
WantedBy=default.target
`)},
		"manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  labels:
    app: test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: test
          image: "{{index .Images "traefik"}}"
          ports:
            - containerPort: {{index .Ports "alloy_http"}}
`)},
	}
}

func TestResolveHost(t *testing.T) {
	t.Parallel()
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	assert.Equal(t, "test-host", resolved.Hostname)
	require.Len(t, resolved.Files, 3)

	var net, cont, manifest ResolvedFile
	for _, f := range resolved.Files {
		switch {
		case strings.HasSuffix(f.DestPath, "internal.network"):
			net = f
		case strings.HasSuffix(f.DestPath, "test.container"):
			cont = f
		case strings.HasSuffix(f.DestPath, "deployment.yml"):
			manifest = f
		}
	}

	// Check network file (static)
	assert.Equal(t, config.CategoryNetwork, net.Category)
	assert.Contains(t, net.Content, "Internal=true")

	// Check container file (templated)
	assert.Equal(t, config.CategoryContainer, cont.Category)
	assert.Contains(t, cont.Content, "Image=traefik:v3.6.9")
	assert.Equal(t, "/etc/containers/systemd/picolet/test.container", cont.DestPath)

	// Check manifest (templated)
	assert.Equal(t, config.CategoryManifest, manifest.Category)
	assert.Contains(t, manifest.Content, "image: \"traefik:v3.6.9\"")
	assert.Contains(t, manifest.Content, "containerPort: 12345")
}

func TestResolveHostNotFound(t *testing.T) {
	t.Parallel()
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	_, err = r.ResolveHost(t.Context(), "nonexistent")
	require.Error(t, err)
}

func TestTemplateDataFields(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Fleet: &config.FleetConfig{
			Images: map[string]string{"test": "img:v1"},
			Ports:  map[string]int{"test": 8080},
		},
		Hosts: map[string]*config.HostConfig{
			"server-host": {
				Hostname:         "server-host",
				ExternalHostname: "server.ts.net",
				PiType:           "server",
				Features:         []string{"mosquitto"},
			},
			"gateway-host": {
				Hostname:         "gateway-host",
				ExternalHostname: "gateway.ts.net",
				PiType:           "monitoring_server",
			},
		},
	}

	t.Run("server host", func(t *testing.T) {
		t.Parallel()
		data, err := NewTemplateData(cfg, "server-host")
		require.NoError(t, err)
		assert.Equal(t, "server", data.Host.PiType)
		assert.Equal(t, "server.ts.net", data.Host.ExternalHostname)
		assert.Contains(t, data.Host.Features, "mosquitto")
		assert.Len(t, data.Fleet.Hosts, 2)
	})

	t.Run("gateway host", func(t *testing.T) {
		t.Parallel()
		data, err := NewTemplateData(cfg, "gateway-host")
		require.NoError(t, err)
		assert.Equal(t, "monitoring_server", data.Host.PiType)
		assert.Equal(t, "gateway.ts.net", data.Host.ExternalHostname)
	})
}

func TestRenderTemplateRecursion(t *testing.T) {
	t.Parallel()
	// Two templates that reference each other to trigger infinite recursion.
	fsys := fstest.MapFS{
		"a.tmpl": &fstest.MapFile{Data: []byte(`{{renderTemplate "b.tmpl" .}}`)},
		"b.tmpl": &fstest.MapFile{Data: []byte(`{{renderTemplate "a.tmpl" .}}`)},
	}
	registry, _, err := BuildRegistry(t.Context(), fsys, nil, nil, "/var/lib/picolet")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = registry.ExecuteTemplate(&buf, "a.tmpl", nil)
	require.Error(t, err) // recursion limit

	require.ErrorContains(t, err, "recursion depth exceeded")
}

func TestRootlessPaths(t *testing.T) {
	t.Parallel()
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg, Rootless: true})
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	for _, f := range resolved.Files {
		if f.Category == "secret" {
			continue
		}
		assert.NotContains(t, f.DestPath, "/etc/", "rootless path should not use /etc/")
		assert.NotContains(t, f.DestPath, "/var/lib/", "rootless path should not use /var/lib/")
	}

	// Find files by suffix to avoid depending on slice order
	var cont, manifest ResolvedFile
	for _, f := range resolved.Files {
		switch {
		case strings.HasSuffix(f.DestPath, "test.container"):
			cont = f
		case strings.HasSuffix(f.DestPath, "deployment.yml"):
			manifest = f
		}
	}

	// Verify container file goes to rootless quadlet dir
	assert.Equal(t, filepath.Join(home, ".config", "containers", "systemd", "picolet", "test.container"), cont.DestPath)

	// Verify manifest goes to rootless data dir
	assert.Equal(t, filepath.Join(home, ".local", "share", "picolet", "manifests", "app", "deployment.yml"), manifest.DestPath)
}

// TestNewConfigDirOverrides verifies that non-empty Config.QuadletDir and
// Config.DataDir take precedence over ResolveDirs defaults. Empty fields
// (i.e. SystemdDir here, plus the "all empty" case) are exercised by
// TestRootlessPaths.
//
// Fixture dependency: testdata/example-fleet must assign a .container file
// and a manifest under "manifests/" to the "test-host" host. Adjust the
// suffix matches below if that ever changes.
func TestNewConfigDirOverrides(t *testing.T) {
	t.Parallel()
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	const customQuadlet = "/custom/quadlet"
	const customData = "/custom/data"

	r, err := New(Config{
		FS:         fsys,
		Config:     cfg,
		Rootless:   true,
		QuadletDir: customQuadlet,
		DataDir:    customData,
		// SystemdDir intentionally left empty — falls back to ResolveDirs.
	})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	defaultSystemdDir := filepath.Join(home, ".config", "systemd", "user")

	var sawContainer, sawManifest, sawSystemd bool
	for _, f := range resolved.Files {
		switch f.Category {
		case "container", "network", "volume", "kube":
			assert.True(t,
				strings.HasPrefix(f.DestPath, customQuadlet+string(filepath.Separator)),
				"%s file %s must live under QuadletDir override", f.Category, f.DestPath)
			sawContainer = true
		case "manifest":
			assert.True(t,
				strings.HasPrefix(f.DestPath, customData+string(filepath.Separator)),
				"manifest file %s must live under DataDir override", f.DestPath)
			sawManifest = true
		case "systemd":
			assert.True(t,
				strings.HasPrefix(f.DestPath, defaultSystemdDir+string(filepath.Separator)),
				"systemd file %s must fall back to default SystemdDir", f.DestPath)
			sawSystemd = true
		}
	}
	assert.True(t, sawContainer, "fixture must produce at least one quadlet file")
	assert.True(t, sawManifest, "fixture must produce at least one manifest file")
	// sawSystemd is best-effort: not every fixture host has a systemd-category file.
	_ = sawSystemd
}

// newHostDataDirFleetFS builds a minimal fleet whose container template uses the
// filePath helper, plus a files/ bundle entry, for host_data_dir coverage.
func newHostDataDirFleetFS() fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  containers:
    - quadlets/app.container.tmpl
  files:
    - files/app.conf
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
pi_type: server
features: []
`)},
		"quadlets/app.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
ContainerName=app
Image=app:latest
Volume={{ filePath "app.conf" }}:/etc/app.conf:ro
`)},
		"files/app.conf": &fstest.MapFile{Data: []byte("key = value\n")},
	}
}

// TestResolveHostHostDataDirAffectsFilePathHelperOnly verifies that HostDataDir
// changes the path emitted by the filePath template helper without changing the
// DestPath where picolet writes the bundled file.
func TestResolveHostHostDataDirAffectsFilePathHelperOnly(t *testing.T) {
	t.Parallel()
	fsys := newHostDataDirFleetFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{
		FS:          fsys,
		Config:      cfg,
		DataDir:     "/internal",
		HostDataDir: "/host",
	})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	var container, file *ResolvedFile
	for i := range resolved.Files {
		switch resolved.Files[i].Category {
		case config.CategoryContainer:
			container = &resolved.Files[i]
		case config.CategoryFile:
			file = &resolved.Files[i]
		}
	}
	require.NotNil(t, container, "fixture must produce a container file")
	require.NotNil(t, file, "fixture must produce a file-category file")

	// filePath helper emits the host-visible path.
	assert.Contains(t, container.Content, "Volume=/host/files/app.conf:/etc/app.conf:ro")
	// The bundled file is still written to picolet's internal data dir.
	assert.Equal(t, "/internal/files/app.conf", file.DestPath)
}

// TestResolveHostHostDataDirDefaultsToDataDir verifies that an unset HostDataDir
// leaves filePath emitting the internal data dir (backward compatibility).
func TestResolveHostHostDataDirDefaultsToDataDir(t *testing.T) {
	t.Parallel()
	fsys := newHostDataDirFleetFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg, DataDir: "/internal"})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	var container *ResolvedFile
	for i := range resolved.Files {
		if resolved.Files[i].Category == config.CategoryContainer {
			container = &resolved.Files[i]
		}
	}
	require.NotNil(t, container)
	assert.Contains(t, container.Content, "Volume=/internal/files/app.conf:/etc/app.conf:ro")
}

//nolint:funlen // fixture setup is clearer inline for equivalence coverage
func TestResolveHostBundleEquivalentToExplicit(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports:
  http: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  bundled:
    services:
      - web
  explicit:
    networks:
      - quadlets/networks/internal.network
    containers:
      - quadlets/containers/web.container.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
features: {}
`)},
		"hosts/bundled/host.yml": &fstest.MapFile{Data: []byte(`
hostname: bundled
external_hostname: bundled.example.net
pi_type: bundled
features: []
`)},
		"hosts/explicit/host.yml": &fstest.MapFile{Data: []byte(`
hostname: explicit
external_hostname: explicit.example.net
pi_type: explicit
features: []
`)},
		"services/web/networks/internal.network": &fstest.MapFile{Data: []byte(`[Network]
Internal=true
`)},
		"quadlets/networks/internal.network": &fstest.MapFile{Data: []byte(`[Network]
Internal=true
`)},
		"services/web/containers/web.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image={{index .Images "app"}}
ContainerName=web
Network=internal.network

[Install]
WantedBy=default.target
`)},
		"quadlets/containers/web.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image={{index .Images "app"}}
ContainerName=web
Network=internal.network

[Install]
WantedBy=default.target
`)},
		"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: web
data:
  image: {{index .Images "app"}}
`)},
		"manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: web
data:
  image: {{index .Images "app"}}
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	bundled, err := r.ResolveHost(t.Context(), "bundled")
	require.NoError(t, err)
	explicit, err := r.ResolveHost(t.Context(), "explicit")
	require.NoError(t, err)

	assert.Equal(t, resolvedOutputs(explicit.Files), resolvedOutputs(bundled.Files))
}

func TestResolveHostRendersServiceSecretHooks(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images: {}
ports:
  app: 1234
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    services:
      - app
features: {}
`)},
		"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
		"services/app/containers/app.container": &fstest.MapFile{Data: []byte(`[Container]
Image=app
ContainerName=app
`)},
		"services/app/secrets/app_config.yml": &fstest.MapFile{Data: []byte("a: 1\n")},
		"services/app/picolet.yml.tmpl": &fstest.MapFile{Data: []byte(`
hooks:
  - name: app-reload
    secrets: [app_config]
    unit: app.service
    action: http
    method: GET
    url: "http://localhost:{{ index .Ports "app" }}/-/reload"
    health_url: "http://localhost:{{ index .Ports "app" }}/-/healthy"
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "server")
	require.NoError(t, err)
	require.Len(t, resolved.Hooks, 1)
	assert.Equal(t, config.Hook{
		Name:       "app-reload",
		Secrets:    []string{"app_config"},
		Unit:       "app.service",
		Action:     config.HookActionHTTP,
		Method:     "GET",
		URL:        "http://localhost:1234/-/reload",
		HealthURL:  "http://localhost:1234/-/healthy",
		OnFailure:  config.HookOnFailureKeepRunning,
		MaxRetries: config.DefaultMaxRetries,
	}, resolved.Hooks[0])
}

func TestResolveHostRejectsInvalidSecretHook(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    services: [app]
features: {}
`)},
		"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
		"services/app/containers/app.container": &fstest.MapFile{Data: []byte("[Container]\nImage=app\n")},
		"services/app/picolet.yml": &fstest.MapFile{Data: []byte(`
hooks:
  - name: broken
    secrets: [app_config]
    unit: app.service
    action: http
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "server")
	require.Error(t, err)
	assert.ErrorContains(t, err, "url is required")
}

func TestResolveHostBatchesOpRefsFromHookTemplates(t *testing.T) {
	t.Parallel()

	hookOpRef := "op://vault/app/reload-token"

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    services: [app]
features: {}
`)},
		"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
		"services/app/containers/app.container": &fstest.MapFile{Data: []byte("[Container]\nImage=app\n")},
		// Hook URL embeds an op:// reference. Phase 1 must collect this ref so
		// the OpSecretReader sees it on the (single) batch call.
		"services/app/picolet.yml.tmpl": &fstest.MapFile{Data: []byte(`
hooks:
  - name: app-reload
    secrets: [app_config]
    unit: app.service
    action: http
    method: POST
    url: "http://example.test/reload?token={{readOpSecret "` + hookOpRef + `"}}"
`)},
	}

	var calls atomic.Int32
	reader := func(_ context.Context, refs []string) (map[string]string, error) {
		calls.Add(1)
		assert.Contains(t, refs, hookOpRef, "hook template's op:// ref must appear in Phase 1 batch")
		return map[string]string{hookOpRef: "tok"}, nil
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "server")
	require.NoError(t, err)
	require.Len(t, resolved.Hooks, 1)
	assert.Equal(t, "http://example.test/reload?token=tok", resolved.Hooks[0].URL)
	assert.Equal(t, int32(1), calls.Load(), "OpSecretReader should be called exactly once for the batch")
}

func TestResolveHostHookNameUniquenessErrorIncludesService(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    services: [app, api]
features: {}
`)},
		"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
		"services/app/containers/app.container": &fstest.MapFile{Data: []byte("[Container]\nImage=app\n")},
		"services/app/picolet.yml": &fstest.MapFile{Data: []byte(`
hooks:
  - name: shared
    secrets: [cfg]
    unit: app.service
    action: http
    url: "http://example.test/reload"
`)},
		"services/api/containers/api.container": &fstest.MapFile{Data: []byte("[Container]\nImage=api\n")},
		"services/api/picolet.yml": &fstest.MapFile{Data: []byte(`
hooks:
  - name: shared
    secrets: [cfg]
    unit: api.service
    action: http
    url: "http://example.test/reload"
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "server")
	require.ErrorContains(t, err, `duplicate hook name "shared"`)
	// The error must name the previously-defining service so the operator can
	// find both files. Hook refs are sorted by service name during bundle
	// expansion, so "api" is processed first and "app" reports the duplicate.
	require.ErrorContains(t, err, `service "api"`)
}

func TestIsQuadletUnit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		unit string
		want bool
	}{
		{"foo.container", true},
		{"foo.network", true},
		{"foo.volume", true},
		{"foo.kube", true},
		{"foo.pod", true},
		{"foo.image", true},
		{"foo.build", true},
		{"foo.artifact", true},
		{"foo.service", false},
		{"foo.timer", false},
		{"", false},
		{"container", false},
	}

	for _, tt := range tests {
		t.Run(tt.unit, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isQuadletUnit(tt.unit))
		})
	}
}

func TestResolveHostHookUnitResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hookUnit string
		wantUnit string
	}{
		{
			name:     "quadlet container resolves to service name",
			hookUnit: "app.container",
			wantUnit: "app.service",
		},
		{
			name:     "explicit service passes through unchanged",
			hookUnit: "app.service",
			wantUnit: "app.service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{
				"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
				"assignments.yml": &fstest.MapFile{Data: []byte("base: {}\npi_types:\n  server:\n    services: [app]\nfeatures: {}\n")},
				"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
				"services/app/containers/app.container": &fstest.MapFile{Data: []byte("[Container]\nImage=app\nContainerName=app\n")},
				"services/app/picolet.yml": &fstest.MapFile{Data: []byte(`
hooks:
  - name: app-reload
    secrets: [cfg]
    unit: ` + tt.hookUnit + `
    action: restart
`)},
			}

			cfg, err := config.LoadAll(fsys)
			require.NoError(t, err)
			r, err := New(Config{FS: fsys, Config: cfg})
			require.NoError(t, err)

			resolved, err := r.ResolveHost(t.Context(), "server")
			require.NoError(t, err)
			require.Len(t, resolved.Hooks, 1)
			assert.Equal(t, tt.wantUnit, resolved.Hooks[0].Unit)
		})
	}
}

func TestResolveHostHookUnitQuadletNotFoundErrors(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte("base: {}\npi_types:\n  server:\n    services: [app]\nfeatures: {}\n")},
		"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
		"services/app/containers/app.container": &fstest.MapFile{Data: []byte("[Container]\nImage=app\nContainerName=app\n")},
		"services/app/picolet.yml": &fstest.MapFile{Data: []byte(`
hooks:
  - name: app-reload
    secrets: [cfg]
    unit: nonexistent.container
    action: restart
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "server")
	require.ErrorContains(t, err, `unit "nonexistent.container"`)
	require.ErrorContains(t, err, "no matching quadlet file found")
}

//nolint:funlen // table-driven test with three quadlet fixtures inline
func TestBuildHooksValidatesSignalContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		quadlet  string
		hookYAML string
		wantErr  string
	}{
		{
			name:    "matching ContainerName accepted",
			quadlet: "[Container]\nImage=app\nContainerName=app-prod\n",
			hookYAML: `hooks:
  - name: app-sighup
    secrets: [cfg]
    unit: app.container
    action: signal
    container: app-prod
    signal: HUP
`,
		},
		{
			name:    "mismatching ContainerName rejected",
			quadlet: "[Container]\nImage=app\nContainerName=app-prod\n",
			hookYAML: `hooks:
  - name: app-sighup
    secrets: [cfg]
    unit: app.container
    action: signal
    container: bar
    signal: HUP
`,
			wantErr: `container "bar" does not match Quadlet ContainerName "app-prod"`,
		},
		{
			name:    "unset ContainerName accepts any container",
			quadlet: "[Container]\nImage=app\n",
			hookYAML: `hooks:
  - name: app-sighup
    secrets: [cfg]
    unit: app.container
    action: signal
    container: anything
    signal: HUP
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
				"assignments.yml": &fstest.MapFile{Data: []byte("base: {}\npi_types:\n  server:\n    services: [app]\nfeatures: {}\n")},
				"hosts/server/host.yml": &fstest.MapFile{Data: []byte(`
hostname: server
pi_type: server
features: []
`)},
				"services/app/containers/app.container": &fstest.MapFile{Data: []byte(tt.quadlet)},
				"services/app/picolet.yml":              &fstest.MapFile{Data: []byte(tt.hookYAML)},
			}
			cfg, err := config.LoadAll(fsys)
			require.NoError(t, err)
			r, err := New(Config{FS: fsys, Config: cfg})
			require.NoError(t, err)
			_, err = r.ResolveHost(t.Context(), "server")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestResolveHostBundleManifestTemplateRenders(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v2"
ports:
  http: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte(`kind: ConfigMap
data:
  image: {{index .Images "app"}}
  host: {{.Host.Hostname}}
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	manifest := findByDest(t, resolved.Files, "/var/lib/picolet/manifests/app/deployment.yml")
	assert.Equal(t, "services/web/manifests/app/deployment.yml.tmpl", manifest.SrcPath)
	assert.Equal(t, "app/deployment.yml", manifest.RelPath)
	assert.Contains(t, manifest.Content, "image: app:v2")
	assert.Contains(t, manifest.Content, "host: test-host")
}

func TestResolveHostBundleFileTemplateUsesDeployedRelPath(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images: {}
ports:
  http: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/files/config/scrape.yml.tmpl": &fstest.MapFile{Data: []byte(`target: {{ .Host.Hostname }}:{{ index .Ports "http" }}
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	file := findByDest(t, resolved.Files, "/var/lib/picolet/files/config/scrape.yml")
	assert.Equal(t, "services/web/files/config/scrape.yml.tmpl", file.SrcPath)
	assert.Equal(t, config.CategoryFile, file.Category)
	assert.Equal(t, "config/scrape.yml", file.RelPath)
	assert.Contains(t, file.Content, "target: test-host:8080")
}

func TestResolveHostBundleManifestNormalizesPath(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/manifests/app/deployment.yml": &fstest.MapFile{Data: []byte("kind: ConfigMap\n")},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	manifest := findByDest(t, resolved.Files, "/var/lib/picolet/manifests/app/deployment.yml")
	assert.Equal(t, "services/web/manifests/app/deployment.yml", manifest.SrcPath)
	assert.NotContains(t, manifest.DestPath, "services/web")
}

func TestResolveHostBundleAndLegacyMixed(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports: {}
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
  systemd:
    - systemd/http.socket
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/containers/web.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image={{index .Images "app"}}
ContainerName=web

[Install]
WantedBy=default.target
`)},
		"systemd/http.socket": &fstest.MapFile{Data: []byte(`[Socket]
ListenStream=8080
`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	assert.Len(t, resolved.Files, 2)
	findByDest(t, resolved.Files, "/etc/containers/systemd/picolet/web.container")
	findByDest(t, resolved.Files, "/etc/systemd/system/http.socket")
}

//nolint:funlen // table-driven collision matrix is intentionally explicit
func TestResolveHostCollision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		assignmentsYAML string
		files           fstest.MapFS
		wantDest        string
		wantSources     []string
	}{
		{
			name: "cross-source quadlet",
			assignmentsYAML: `
base:
  services:
    - web
  containers:
    - quadlets/containers/web.container.tmpl
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/containers/web.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image=app:v1
ContainerName=web

[Install]
WantedBy=default.target
`)},
				"quadlets/containers/web.container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image=app:v2
ContainerName=web

[Install]
WantedBy=default.target
`)},
			},
			wantDest: "/etc/containers/systemd/picolet/web.container",
			wantSources: []string{
				"quadlets/containers/web.container.tmpl",
				"services/web/containers/web.container.tmpl",
			},
		},
		{
			name: "cross-source systemd",
			assignmentsYAML: `
base:
  services:
    - web
  systemd:
    - systemd/http.socket
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/systemd/http.socket": &fstest.MapFile{Data: []byte("[Socket]\nListenStream=8080\n")},
				"systemd/http.socket":              &fstest.MapFile{Data: []byte("[Socket]\nListenStream=9090\n")},
			},
			wantDest: "/etc/systemd/system/http.socket",
			wantSources: []string{
				"services/web/systemd/http.socket",
				"systemd/http.socket",
			},
		},
		{
			name: "cross-source manifest",
			assignmentsYAML: `
base:
  services:
    - web
  manifests:
    - manifests/app/deployment.yml.tmpl
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte("kind: ConfigMap\n")},
				"manifests/app/deployment.yml.tmpl":              &fstest.MapFile{Data: []byte("kind: Secret\n")},
			},
			wantDest: "/var/lib/picolet/manifests/app/deployment.yml",
			wantSources: []string{
				"manifests/app/deployment.yml.tmpl",
				"services/web/manifests/app/deployment.yml.tmpl",
			},
		},
		{
			name: "cross-source secret",
			assignmentsYAML: `
base:
  services:
    - web
  secrets:
    - secrets/config.yaml
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/secrets/config.yml": &fstest.MapFile{Data: []byte("a: 1\n")},
				"secrets/config.yaml":             &fstest.MapFile{Data: []byte("a: 2\n")},
			},
			wantDest: "secret:config",
			wantSources: []string{
				"secrets/config.yaml",
				"services/web/secrets/config.yml",
			},
		},
		{
			name: "within-bundle quadlet tmpl variant",
			assignmentsYAML: `
base:
  services:
    - web
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/containers/web.container":      &fstest.MapFile{Data: []byte("[Container]\nImage=a\nContainerName=web\n")},
				"services/web/containers/web.container.tmpl": &fstest.MapFile{Data: []byte("[Container]\nImage=b\nContainerName=web\n")},
			},
			wantDest: "/etc/containers/systemd/picolet/web.container",
			wantSources: []string{
				"services/web/containers/web.container",
				"services/web/containers/web.container.tmpl",
			},
		},
		{
			name: "within-bundle cross-category quadlet",
			assignmentsYAML: `
base:
  services:
    - web
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\nContainerName=web\n")},
				"services/web/volumes/web.container":    &fstest.MapFile{Data: []byte("[Volume]\n")},
			},
			wantDest: "/etc/containers/systemd/picolet/web.container",
			wantSources: []string{
				"services/web/containers/web.container",
				"services/web/volumes/web.container",
			},
		},
		{
			name: "within-bundle secret extension",
			assignmentsYAML: `
base:
  services:
    - web
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/secrets/config.yml":  &fstest.MapFile{Data: []byte("a: 1\n")},
				"services/web/secrets/config.yaml": &fstest.MapFile{Data: []byte("a: 2\n")},
			},
			wantDest: "secret:config",
			wantSources: []string{
				"services/web/secrets/config.yaml",
				"services/web/secrets/config.yml",
			},
		},
		{
			name: "within-bundle manifest tmpl variant",
			assignmentsYAML: `
base:
  services:
    - web
pi_types: {}
features: {}
`,
			files: fstest.MapFS{
				"services/web/manifests/app/deployment.yml":      &fstest.MapFile{Data: []byte("kind: ConfigMap\n")},
				"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte("kind: Secret\n")},
			},
			wantDest: "/var/lib/picolet/manifests/app/deployment.yml",
			wantSources: []string{
				"services/web/manifests/app/deployment.yml",
				"services/web/manifests/app/deployment.yml.tmpl",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
				"assignments.yml": &fstest.MapFile{Data: []byte(tt.assignmentsYAML)},
				"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
			}
			maps.Copy(fsys, tt.files)

			cfg, err := config.LoadAll(fsys)
			require.NoError(t, err)
			r, err := New(Config{FS: fsys, Config: cfg})
			require.NoError(t, err)

			_, err = r.ResolveHost(t.Context(), "test-host")
			require.ErrorContains(t, err, "destination collision")
			require.ErrorContains(t, err, tt.wantDest)
			for _, src := range tt.wantSources {
				assert.ErrorContains(t, err, src)
			}
		})
	}
}

// TestResolveHostCollisionFastFail pins the regression: a collision must be
// detected before any 1Password SDK call fires. If the pre-render collision
// check is ever moved back behind template rendering, the op reader would be
// invoked and fail this test.
func TestResolveHostCollisionFastFail(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
  secrets:
    - secrets/config.yaml
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/secrets/config.yml": &fstest.MapFile{Data: []byte("a: 1\n")},
		"secrets/config.yaml":             &fstest.MapFile{Data: []byte("a: 2\n")},
	}

	readerCalled := false
	reader := func(_ context.Context, _ []string) (map[string]string, error) {
		readerCalled = true
		t.Fatal("1Password reader must not be called when a destination collision is detectable")
		return nil, errors.New("unreachable")
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "test-host")
	require.ErrorContains(t, err, "destination collision")
	assert.False(t, readerCalled, "1Password reader must not be invoked on collision")
}

func TestResolveHostLegacyAssignmentsUnchanged(t *testing.T) {
	t.Parallel()

	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	assert.Equal(t, []resolvedTuple{
		{
			SrcPath:  "quadlets/networks/internal.network",
			DestPath: "/etc/containers/systemd/picolet/internal.network",
			Category: "network",
			Content:  "[Network]\nInternal=true\n",
		},
		{
			SrcPath:  "quadlets/containers/test.container.tmpl",
			DestPath: "/etc/containers/systemd/picolet/test.container",
			Category: "container",
			Content:  "[Container]\nContainerName=test\nImage=traefik:v3.6.9\nNetwork=internal.network\n\n[Install]\nWantedBy=default.target\n",
		},
		{
			SrcPath:  "manifests/app/deployment.yml.tmpl",
			DestPath: "/var/lib/picolet/manifests/app/deployment.yml",
			Category: "manifest",
			Content:  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: test-app\n  labels:\n    app: test\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: test\n  template:\n    metadata:\n      labels:\n        app: test\n    spec:\n      containers:\n        - name: test\n          image: \"traefik:v3.6.9\"\n          ports:\n            - containerPort: 12345\n",
		},
	}, resolvedFileTuples(resolved.Files))
}

func TestResolveHostBundledManifestCollectsOpSecrets(t *testing.T) {
	t.Parallel()

	const ref = "op://vault/item/password"
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  services:
    - web
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.example.net
pi_type: node
features: []
`)},
		"services/web/manifests/app/secret.yml.tmpl": &fstest.MapFile{Data: []byte(`kind: Secret
stringData:
  password: {{readOpSecret "` + ref + `"}}
`)},
	}

	reader := func(_ context.Context, refs []string) (map[string]string, error) {
		require.Equal(t, []string{ref}, refs)
		return map[string]string{ref: "resolved-password"}, nil
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	manifest := findByDest(t, resolved.Files, "/var/lib/picolet/manifests/app/secret.yml")
	assert.Contains(t, manifest.Content, "password: resolved-password")
}

type resolvedTuple struct {
	SrcPath  string
	DestPath string
	Category config.Category
	Content  string
}

type resolvedOutput struct {
	DestPath string
	Category config.Category
	Content  string
}

func resolvedFileTuples(files []ResolvedFile) []resolvedTuple {
	tuples := make([]resolvedTuple, 0, len(files))
	for _, file := range files {
		tuples = append(tuples, resolvedTuple{
			SrcPath:  file.SrcPath,
			DestPath: file.DestPath,
			Category: file.Category,
			Content:  file.Content,
		})
	}
	slices.SortFunc(tuples, func(a, b resolvedTuple) int {
		if diff := strings.Compare(a.DestPath, b.DestPath); diff != 0 {
			return diff
		}
		return strings.Compare(a.SrcPath, b.SrcPath)
	})
	return tuples
}

func resolvedOutputs(files []ResolvedFile) []resolvedOutput {
	outputs := make([]resolvedOutput, 0, len(files))
	for _, file := range files {
		outputs = append(outputs, resolvedOutput{
			DestPath: file.DestPath,
			Category: file.Category,
			Content:  file.Content,
		})
	}
	slices.SortFunc(outputs, func(a, b resolvedOutput) int {
		if diff := strings.Compare(a.DestPath, b.DestPath); diff != 0 {
			return diff
		}
		return strings.Compare(a.Category.String(), b.Category.String())
	})
	return outputs
}

func newSecretTestFS(tb testing.TB) (fstest.MapFS, *config.Config) {
	tb.Helper()
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports:
  app: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  secrets:
    - secrets/static_config.yml
    - secrets/rendered.yml.tmpl
    - secrets/host_only.txt
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
		// Static repo secret — should be copied as-is
		"secrets/static_config.yml": &fstest.MapFile{Data: []byte(`groups:
  - alert: InstanceDown
    annotations:
      summary: "{{ $labels.job }} is down"
`)},
		// Template secret — should be rendered
		"secrets/rendered.yml.tmpl": &fstest.MapFile{Data: []byte(`endpoint: https://{{ .Host.ExternalHostname }}:{{ index .Ports "app" }}
`)},
		// host_only.txt is NOT in the repo FS — should fall through to secretReader
	}
	cfg, err := config.LoadAll(fsys)
	require.NoError(tb, err)
	return fsys, cfg
}

func findByDest(tb testing.TB, files []ResolvedFile, dest string) ResolvedFile {
	tb.Helper()
	for _, f := range files {
		if f.DestPath == dest {
			return f
		}
	}
	tb.Fatalf("no file with DestPath %q found", dest)
	return ResolvedFile{}
}

func TestStaticRepoSecret(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:static_config")
	assert.Equal(t, config.CategorySecret, f.Category)
	assert.Equal(t, string(fsys["secrets/static_config.yml"].Data), f.Content,
		"static secret must be copied verbatim without template rendering")
}

func TestTemplateSecret(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:rendered")
	assert.Equal(t, config.CategorySecret, f.Category)
	assert.Contains(t, f.Content, "endpoint: https://test-host.ts.net:8080")
}

func TestHostOnlySecretWithReader(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	reader := func(path string) (string, error) {
		if path == "host_only.txt" {
			return "host-secret-data", nil
		}
		return "", fmt.Errorf("unknown secret: %s", path)
	}
	r, err := New(Config{FS: fsys, Config: cfg, SecretReader: reader})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:host_only")
	assert.Equal(t, config.CategorySecret, f.Category)
	assert.Equal(t, "host-secret-data", f.Content)
}

func TestHostOnlySecretPlaceholder(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:host_only")
	assert.Equal(t, "<secret>", f.Content)
}

func TestStaticRepoSecretReadError(t *testing.T) {
	t.Parallel()
	fsys, _ := newSecretTestFS(t)

	// Override assignments to reference only a broken secret, and add a
	// directory entry where a file is expected (non-ErrNotExist read error).
	fsys["assignments.yml"] = &fstest.MapFile{Data: []byte(`
base:
  secrets:
    - secrets/broken.yml
pi_types: {}
features: {}
`)}
	fsys["secrets/broken.yml"] = &fstest.MapFile{Mode: fs.ModeDir}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "test-host")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading static secret")
}

func TestSecretPathTraversal(t *testing.T) {
	t.Parallel()
	secretsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "valid.txt"), []byte("secret"), 0o600))

	secretRoot, err := os.OpenRoot(secretsDir)
	require.NoError(t, err)
	defer secretRoot.Close()

	// Valid read should succeed
	data, err := secretRoot.ReadFile("valid.txt")
	require.NoError(t, err)
	assert.Equal(t, "secret", string(data))

	// Path traversal should fail
	_, err = secretRoot.ReadFile("../../etc/passwd")
	require.Error(t, err)
}

//nolint:funlen // table-driven test subtests
func TestReadOpSecret(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"secret.tmpl": &fstest.MapFile{Data: []byte(`pw={{readOpSecret "op://vault/item/pw"}}`)},
	}

	t.Run("with reader two-phase", func(t *testing.T) {
		t.Parallel()
		reader := func(_ context.Context, refs []string) (map[string]string, error) {
			results := make(map[string]string, len(refs))
			for _, ref := range refs {
				if ref == "op://vault/item/pw" {
					results[ref] = "s3cret"
				} else {
					return nil, fmt.Errorf("unknown ref: %s", ref)
				}
			}
			return results, nil
		}
		registry, caches, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{OpProvider(reader)}, "/var/lib/picolet")
		require.NoError(t, err)
		require.Len(t, caches, 1)
		cache := caches[ProviderOnePassword]
		require.NotNil(t, cache)

		// Phase 1: collect (output discarded).
		var discard bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&discard, "secret.tmpl", nil))
		assert.Equal(t, "pw=<op-secret-pending>", discard.String())

		// Batch resolve.
		require.NoError(t, cache.Resolve(t.Context()))

		// Phase 2: resolve (output used).
		var buf bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&buf, "secret.tmpl", nil))
		assert.Equal(t, "pw=s3cret", buf.String())
	})

	t.Run("nil reader returns placeholder", func(t *testing.T) {
		t.Parallel()
		registry, caches, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{OpProvider(nil)}, "/var/lib/picolet")
		require.NoError(t, err)
		assert.Empty(t, caches)
		var buf bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&buf, "secret.tmpl", nil))
		assert.Equal(t, "pw=<op-secret>", buf.String())
	})

	t.Run("reader error propagates via cache resolve", func(t *testing.T) {
		t.Parallel()
		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, fmt.Errorf("1password error")
		}
		registry, caches, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{OpProvider(reader)}, "/var/lib/picolet")
		require.NoError(t, err)
		cache := caches[ProviderOnePassword]

		// Collect phase.
		var discard bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&discard, "secret.tmpl", nil))

		// Resolve fails.
		err = cache.Resolve(t.Context())
		require.Error(t, err)
		assert.ErrorContains(t, err, "1password error")
	})

	t.Run("invalid ref returns error in collect phase", func(t *testing.T) {
		t.Parallel()
		invalidFS := fstest.MapFS{
			"bad.tmpl": &fstest.MapFile{Data: []byte(`pw={{readOpSecret "not-an-op-ref"}}`)},
		}
		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, fmt.Errorf("should-not-be-called")
		}
		registry, _, err := BuildRegistry(t.Context(), invalidFS, nil, []ProviderTemplate{OpProvider(reader)}, "/var/lib/picolet")
		require.NoError(t, err)
		var buf bytes.Buffer
		err = registry.ExecuteTemplate(&buf, "bad.tmpl", nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, "is not a valid reference")
	})

	t.Run("batches multiple refs in single call", func(t *testing.T) {
		t.Parallel()
		multiFS := fstest.MapFS{
			"multi.tmpl": &fstest.MapFile{Data: []byte(
				`a={{readOpSecret "op://v/a/f"}} b={{readOpSecret "op://v/b/f"}}`,
			)},
		}
		var callCount int
		reader := func(_ context.Context, refs []string) (map[string]string, error) {
			callCount++
			results := make(map[string]string, len(refs))
			for _, ref := range refs {
				results[ref] = "val-" + ref
			}
			return results, nil
		}
		registry, caches, err := BuildRegistry(t.Context(), multiFS, nil, []ProviderTemplate{OpProvider(reader)}, "/var/lib/picolet")
		require.NoError(t, err)
		cache := caches[ProviderOnePassword]

		// Collect.
		var discard bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&discard, "multi.tmpl", nil))

		// Batch resolve — single call for both refs.
		require.NoError(t, cache.Resolve(t.Context()))
		assert.Equal(t, 1, callCount, "expected single batch call, got %d", callCount)

		// Resolve.
		var buf bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&buf, "multi.tmpl", nil))
		assert.Equal(t, `a=val-op://v/a/f b=val-op://v/b/f`, buf.String())
	})
}

// TestReadProvidersCoexist exercises the two-phase resolution machinery with
// both providers active simultaneously and a template that calls each.
func TestReadProvidersCoexist(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"both.tmpl": &fstest.MapFile{Data: []byte(
			`op={{readOpSecret "op://v/i/f"}} pp={{readProtonPassSecret "pass://s/i/f"}}`,
		)},
	}

	opReader := func(_ context.Context, refs []string) (map[string]string, error) {
		out := make(map[string]string, len(refs))
		for _, r := range refs {
			out[r] = "op-" + r
		}
		return out, nil
	}
	ppReader := func(_ context.Context, refs []string) (map[string]string, error) {
		out := make(map[string]string, len(refs))
		for _, r := range refs {
			out[r] = "pp-" + r
		}
		return out, nil
	}

	registry, caches, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{
		OpProvider(opReader),
		PPProvider(ppReader),
	}, "/var/lib/picolet")
	require.NoError(t, err)
	require.Len(t, caches, 2)
	opCache, ppCache := caches[ProviderOnePassword], caches[ProviderProtonPass]
	require.NotNil(t, opCache)
	require.NotNil(t, ppCache)

	// Collect phase.
	var discard bytes.Buffer
	require.NoError(t, registry.ExecuteTemplate(&discard, "both.tmpl", nil))
	assert.Equal(t, "op=<op-secret-pending> pp=<pp-secret-pending>", discard.String())

	// Resolve both caches.
	require.NoError(t, opCache.Resolve(t.Context()))
	require.NoError(t, ppCache.Resolve(t.Context()))

	// Render phase.
	var buf bytes.Buffer
	require.NoError(t, registry.ExecuteTemplate(&buf, "both.tmpl", nil))
	assert.Equal(t, "op=op-op://v/i/f pp=pp-pass://s/i/f", buf.String())
}

func TestResolveHostSkipsOpSecretWhenNotConfigured(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports:
  app: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  secrets:
    - op://vault/item/field
    - secrets/normal.yml
pi_types: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
		"secrets/normal.yml": &fstest.MapFile{Data: []byte("normal-secret-data")},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	// nil OpSecretReader — 1Password not configured.
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	// The op:// secret should be skipped, only the normal secret should be present.
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, "secret:normal", resolved.Files[0].DestPath)
	assert.Equal(t, "normal-secret-data", resolved.Files[0].Content)
}

// newOpSecretTestFS returns a filesystem and config wired for op:// secret tests.
// The only secret in assignments is an op:// ref so tests can control exactly what resolves.
func newOpSecretTestFS(tb testing.TB, secretRef string) (fstest.MapFS, *config.Config) {
	tb.Helper()
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports:
  app: 8080
`)},
		"assignments.yml": &fstest.MapFile{Data: fmt.Appendf(nil, `
base:
  secrets:
    - %s
pi_types: {}
features: {}
`, secretRef)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
	}
	cfg, err := config.LoadAll(fsys)
	require.NoError(tb, err)
	return fsys, cfg
}

//nolint:funlen // table-driven test subtests
func TestResolveOpSecret(t *testing.T) {
	t.Parallel()

	t.Run("with reader resolves secret", func(t *testing.T) {
		t.Parallel()
		const ref = "op://vault/item/field"
		fsys, cfg := newOpSecretTestFS(t, ref)

		reader := func(_ context.Context, refs []string) (map[string]string, error) {
			results := make(map[string]string, len(refs))
			for _, r := range refs {
				if r == ref {
					results[r] = "resolved-value"
				} else {
					return nil, fmt.Errorf("unexpected ref: %s", r)
				}
			}
			return results, nil
		}
		r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
		require.NoError(t, err)

		resolved, err := r.ResolveHost(t.Context(), "test-host")
		require.NoError(t, err)

		require.Len(t, resolved.Files, 1)
		f := resolved.Files[0]
		assert.Equal(t, ref, f.SrcPath)
		assert.Equal(t, "secret:vault_item_field", f.DestPath)
		assert.Equal(t, "resolved-value", f.Content)
		assert.Equal(t, config.CategorySecret, f.Category)
	})

	t.Run("nil reader skips op secret", func(t *testing.T) {
		t.Parallel()
		fsys, cfg := newOpSecretTestFS(t, "op://vault/item/field")

		// No OpSecretReader — op:// secrets are silently skipped by ResolveHost.
		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)

		resolved, err := r.ResolveHost(t.Context(), "test-host")
		require.NoError(t, err)

		// op:// secret must be absent from resolved files.
		assert.Empty(t, resolved.Files)
	})

	t.Run("malformed op ref falls through to regular secret path", func(t *testing.T) {
		t.Parallel()
		// "op://vault/item" has only two path components — field is missing.
		// IsRef rejects it (uses ParseOpRef), so it falls through to the regular
		// secret path. Without a secretReader, it gets a placeholder.
		fsys, cfg := newOpSecretTestFS(t, "op://vault/item")

		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, fmt.Errorf("should-not-be-called")
		}
		r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
		require.NoError(t, err)

		resolved, err := r.ResolveHost(t.Context(), "test-host")
		require.NoError(t, err)
		// Falls through to host-only secret, gets placeholder since no SecretReader.
		require.Len(t, resolved.Files, 1)
		assert.Equal(t, "<secret>", resolved.Files[0].Content)
	})

	t.Run("partial failure aborts to prevent secret deletion", func(t *testing.T) {
		t.Parallel()
		// Two op:// secrets: one succeeds, one fails.
		// Must return an error so reconciler.Diff does not mark the failed secret for deletion.
		fsys := fstest.MapFS{
			"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  app: "app:v1"
ports:
  app: 8080
`)},
			"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  secrets:
    - op://vault/good/field
    - op://vault/bad/field
pi_types: {}
features: {}
`)},
			"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
		}
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)

		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			results := map[string]string{"op://vault/good/field": "good-value"}
			return results, fmt.Errorf("resolving 1password secret %q: fieldNotFound", "op://vault/bad/field")
		}
		r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
		require.NoError(t, err)

		_, err = r.ResolveHost(t.Context(), "test-host")
		require.Error(t, err)
		assert.ErrorContains(t, err, "resolving onepassword secrets")
	})

	t.Run("total failure returns error", func(t *testing.T) {
		t.Parallel()
		fsys, cfg := newOpSecretTestFS(t, "op://vault/item/field")

		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, fmt.Errorf("1password service unavailable")
		}
		r, err := New(Config{FS: fsys, Config: cfg, OpSecretReader: reader})
		require.NoError(t, err)

		_, err = r.ResolveHost(t.Context(), "test-host")
		require.Error(t, err)
		assert.ErrorContains(t, err, "1password service unavailable")
	})
}
