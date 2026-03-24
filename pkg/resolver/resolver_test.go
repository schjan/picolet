package resolver

import (
	"bytes"
	"context"
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
	registry, _, err := BuildRegistry(t.Context(), fsys, nil, nil)
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
	assert.Equal(t, "secret", f.Category)
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

	resolved, err := r.ResolveHost(t.Context(), "test-host")
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

//nolint:funlen,cyclop // subtests cover multiple scenarios
func TestResolveAggregateSecret(t *testing.T) {
	t.Parallel()

	t.Run("basic concatenation with header", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/alert-cpu.yml": &fstest.MapFile{Data: []byte("- name: cpu\n  rules: []\n")},
			"rules/alert-mem.yml": &fstest.MapFile{Data: []byte("- name: mem\n  rules: []\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "prometheus_rules", Glob: "rules/*.yml", Header: "groups:\n"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:prometheus_rules" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg, "expected aggregate secret in resolved files")
		assert.Equal(t, "secret", agg.Category)
		// Header is prepended, files sorted alphabetically
		assert.Equal(t, "groups:\n- name: cpu\n  rules: []\n- name: mem\n  rules: []\n", agg.Content)
	})

	t.Run("no header produces bare concatenation", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/a.yml": &fstest.MapFile{Data: []byte("a-content\n")},
			"rules/b.yml": &fstest.MapFile{Data: []byte("b-content\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "myrules", Glob: "rules/*.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:myrules" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg)
		assert.Equal(t, "a-content\nb-content\n", agg.Content)
	})

	t.Run("glob matches no files returns error", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "missing", Glob: "nonexistent/*.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		_, err = r.ResolveHost("test-host")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "matched no files")
	})

	t.Run("files sorted by path regardless of FS order", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			// z comes before a in insertion order but should be sorted after
			"rules/z.yml": &fstest.MapFile{Data: []byte("z\n")},
			"rules/a.yml": &fstest.MapFile{Data: []byte("a\n")},
			"rules/m.yml": &fstest.MapFile{Data: []byte("m\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "sorted", Glob: "rules/*.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:sorted" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg)
		assert.Equal(t, "a\nm\nz\n", agg.Content)
	})

	t.Run("prometheus template expressions pass through unmodified", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/prom.yml": &fstest.MapFile{Data: []byte("expr: rate(http_requests_total{job=\"api\"}[5m]) > {{ $value }}\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "prom_rules", Glob: "rules/*.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:prom_rules" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg)
		assert.Contains(t, agg.Content, "{{ $value }}")
	})

	t.Run("multiple globs for same name merged into one secret", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/common/watchdog.yml": &fstest.MapFile{Data: []byte("- name: watchdog\n")},
			"rules/monitoring/cpu.yml":  &fstest.MapFile{Data: []byte("- name: cpu\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		// Simulate base + feature both contributing to the same secret name
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "prometheus_rules", Glob: "rules/common/*.yml", Header: "groups:\n"},
		}
		cfg.Assignments.Features = map[string]config.AssignmentGroup{
			"monitoring": {
				AggregateSecrets: []config.AggregateSecret{
					{Name: "prometheus_rules", Glob: "rules/monitoring/*.yml"},
				},
			},
		}

		host := cfg.Hosts["test-host"]
		host.Features = []string{"monitoring"}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var secrets []ResolvedFile
		for _, f := range resolved.Files {
			if f.DestPath == "secret:prometheus_rules" {
				secrets = append(secrets, f)
			}
		}
		// Must produce exactly one secret, not two
		require.Len(t, secrets, 1)
		assert.Contains(t, secrets[0].Content, "groups:\n")
		assert.Contains(t, secrets[0].Content, "- name: watchdog")
		assert.Contains(t, secrets[0].Content, "- name: cpu")
	})

	t.Run("overlapping globs deduplicate matched files", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/host.yml":     &fstest.MapFile{Data: []byte("host\n")},
			"rules/watchdog.yml": &fstest.MapFile{Data: []byte("watchdog\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		// Both globs match rules/host.yml — it must appear only once in the output
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "overlap", Glob: "rules/*.yml"},
			{Name: "overlap", Glob: "rules/host.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:overlap" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg)
		// host.yml content must appear exactly once, not twice
		assert.Equal(t, "host\nwatchdog\n", agg.Content)
	})

	t.Run("missing trailing newline gets separator injected", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"rules/a.yml": &fstest.MapFile{Data: []byte("a-content")}, // no trailing newline
			"rules/b.yml": &fstest.MapFile{Data: []byte("b-content\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "noeol", Glob: "rules/*.yml", Header: "header:"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		resolved, err := r.ResolveHost("test-host")
		require.NoError(t, err)

		var agg *ResolvedFile
		for i := range resolved.Files {
			if resolved.Files[i].DestPath == "secret:noeol" {
				agg = &resolved.Files[i]
				break
			}
		}
		require.NotNil(t, agg)
		// header and a.yml both lack trailing newlines — separators must be injected
		assert.Equal(t, "header:\na-content\nb-content\n", agg.Content)
	})

	t.Run("name collision with regular secret returns error", func(t *testing.T) {
		t.Parallel()
		fsys := addBaseFiles(fstest.MapFS{
			"secrets/myrules.yml": &fstest.MapFile{Data: []byte("static\n")},
			"rules/alert.yml":     &fstest.MapFile{Data: []byte("alert\n")},
		})
		cfg, err := config.LoadAll(fsys)
		require.NoError(t, err)
		// Regular secret resolves to "secret:myrules", aggregate also targets "myrules"
		cfg.Assignments.Base.Secrets = []string{"secrets/myrules.yml"}
		cfg.Assignments.Base.AggregateSecrets = []config.AggregateSecret{
			{Name: "myrules", Glob: "rules/*.yml"},
		}

		r, err := New(Config{FS: fsys, Config: cfg})
		require.NoError(t, err)
		_, err = r.ResolveHost("test-host")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "myrules")
		assert.Contains(t, err.Error(), "both as a regular secret and an aggregate secret")
	})
}

// addBaseFiles adds the minimum fleet.yml, assignments.yml, and host.yml to a MapFS
// so config.LoadAll succeeds. The caller can set cfg.Assignments fields after loading.
func addBaseFiles(fsys fstest.MapFS) fstest.MapFS {
	fsys["fleet.yml"] = &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")}
	fsys["assignments.yml"] = &fstest.MapFile{Data: []byte("base: {}\npi_types: {}\nfeatures: {}\n")}
	fsys["hosts/test-host/host.yml"] = &fstest.MapFile{Data: []byte("hostname: test-host\npi_type: server\n")}
	return fsys
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
		registry, cache, err := BuildRegistry(t.Context(), fsys, nil, reader)
		require.NoError(t, err)
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
		registry, cache, err := BuildRegistry(t.Context(), fsys, nil, nil)
		require.NoError(t, err)
		assert.Nil(t, cache)
		var buf bytes.Buffer
		require.NoError(t, registry.ExecuteTemplate(&buf, "secret.tmpl", nil))
		assert.Equal(t, "pw=<op-secret>", buf.String())
	})

	t.Run("reader error propagates via cache resolve", func(t *testing.T) {
		t.Parallel()
		reader := func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, fmt.Errorf("1password error")
		}
		registry, cache, err := BuildRegistry(t.Context(), fsys, nil, reader)
		require.NoError(t, err)

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
		registry, _, err := BuildRegistry(t.Context(), invalidFS, nil, reader)
		require.NoError(t, err)
		var buf bytes.Buffer
		err = registry.ExecuteTemplate(&buf, "bad.tmpl", nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, "is not a valid op:// reference")
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
		registry, cache, err := BuildRegistry(t.Context(), multiFS, nil, reader)
		require.NoError(t, err)

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
		assert.Equal(t, "secret", f.Category)
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
		assert.ErrorContains(t, err, "resolving 1password secrets")
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
