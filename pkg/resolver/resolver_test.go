package resolver

import (
	"bytes"
	"fmt"
	"io/fs"
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

	resolved, err := r.ResolveHost("test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:static_config")
	assert.Equal(t, "secret", f.Category)
	assert.Equal(t, string(fsys["secrets/static_config.yml"].Data), f.Content,
		"static secret must be copied verbatim without template rendering")
}

func TestTemplateSecret(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost("test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:rendered")
	assert.Equal(t, "secret", f.Category)
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

	resolved, err := r.ResolveHost("test-host")
	require.NoError(t, err)

	f := findByDest(t, resolved.Files, "secret:host_only")
	assert.Equal(t, "secret", f.Category)
	assert.Equal(t, "host-secret-data", f.Content)
}

func TestHostOnlySecretPlaceholder(t *testing.T) {
	t.Parallel()
	fsys, cfg := newSecretTestFS(t)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost("test-host")
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

	_, err = r.ResolveHost("test-host")
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
