package resolver

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
	resolved, err := r.ResolveHost("test-host")
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
	assert.Equal(t, "network", net.Category)
	assert.Contains(t, net.Content, "Internal=true")

	// Check container file (templated)
	assert.Equal(t, "container", cont.Category)
	assert.Contains(t, cont.Content, "Image=traefik:v3.6.9")
	assert.Equal(t, "/etc/containers/systemd/picolet/test.container", cont.DestPath)

	// Check manifest (templated)
	assert.Equal(t, "manifest", manifest.Category)
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
	_, err = r.ResolveHost("nonexistent")
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
	registry, err := BuildRegistry(fsys, nil)
	require.NoError(t, err)

	var buf bytes.Buffer
	err = registry.ExecuteTemplate(&buf, "a.tmpl", nil)
	require.Error(t, err)
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

	resolved, err := r.ResolveHost("test-host")
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

//nolint:funlen // in-memory filesystem construction with sub-test
func TestResolveConfigFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images: {}
ports: {}
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    config_files:
      - src: configfiles/mosquitto.conf.tmpl
        volume: mosquitto-config
        path: mosquitto.conf
        restart_service: mosquitto
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
		"configfiles/mosquitto.conf.tmpl": &fstest.MapFile{Data: []byte(`# host={{ .Host.Hostname }}`)},
	}

	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)

	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost("test-host")
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)

	f := resolved.Files[0]
	assert.Equal(t, "configfile", f.Category)
	assert.Equal(t, "volumefile:mosquitto-config:mosquitto.conf", f.DestPath)
	assert.Equal(t, "# host=test-host", f.Content)
	assert.Equal(t, "mosquitto.service", f.ServiceName)
	assert.Nil(t, f.ParsedUnit, "configfiles should not have a parsed unit")

	t.Run("no restart_service yields empty ServiceName", func(t *testing.T) {
		t.Parallel()
		fsys2 := fstest.MapFS{
			"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {}")},
			"assignments.yml": &fstest.MapFile{Data: []byte(`
base: {}
pi_types:
  server:
    config_files:
      - src: configfiles/mosquitto.conf.tmpl
        volume: mosquitto-config
        path: mosquitto.conf
features: {}
`)},
			"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`
hostname: test-host
external_hostname: test-host.ts.net
pi_type: server
features: []
`)},
			"configfiles/mosquitto.conf.tmpl": &fstest.MapFile{Data: []byte(`static content`)},
		}
		cfg2, err := config.LoadAll(fsys2)
		require.NoError(t, err)
		r2, err := New(Config{FS: fsys2, Config: cfg2})
		require.NoError(t, err)
		resolved2, err := r2.ResolveHost("test-host")
		require.NoError(t, err)
		require.Len(t, resolved2.Files, 1)
		assert.Empty(t, resolved2.Files[0].ServiceName)
	})
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
