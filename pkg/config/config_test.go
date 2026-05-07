package config

import (
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // table-driven test
func TestLoadAll(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte(`
images:
  traefik: "traefik:v3"
  alloy: "alloy:v1"
ports:
  alloy_http: 12345
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
    - quadlets/containers/traefik.container.tmpl
pi_types:
  monitoring_server:
    containers:
      - quadlets/containers/prometheus.container.tmpl
features:
  mosquitto:
    kube:
      - quadlets/kube/mosquitto-stack.kube.tmpl
`)},
		"hosts/host-a/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-a
external_hostname: host-a.ts.net
pi_type: server
features:
  - mosquitto
`)},
		"hosts/host-b/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-b
external_hostname: host-b.ts.net
pi_type: monitoring_server
features: []
`)},
	}

	cfg, err := LoadAll(fsys)
	require.NoError(t, err)

	// Check fleet config
	assert.Equal(t, "traefik:v3", cfg.Fleet.Images["traefik"])
	assert.Equal(t, 12345, cfg.Fleet.Ports["alloy_http"])
	assert.Equal(t, "35d", cfg.Fleet.Prometheus["retention_time"])

	// Check hosts
	require.Len(t, cfg.Hosts, 2)
	assert.Equal(t, "server", cfg.Hosts["host-a"].PiType)
	assert.Equal(t, "monitoring_server", cfg.Hosts["host-b"].PiType)
	assert.Equal(t, "host-a.ts.net", cfg.Hosts["host-a"].ExternalHostname)

	// Check sorted hostnames
	hostnames := cfg.SortedHostnames()
	require.Len(t, hostnames, 2)
	assert.Equal(t, "host-a", hostnames[0])
	assert.Equal(t, "host-b", hostnames[1])
}

//nolint:funlen // table-driven coverage for assignment merge behavior
func TestAssignmentsResolve(t *testing.T) {
	t.Parallel()
	assignments := &Assignments{
		Base: AssignmentGroup{
			Networks:   []string{"net1"},
			Containers: []string{"base-container"},
			Services:   []string{"base-service"},
		},
		PiTypes: map[string]AssignmentGroup{
			"monitoring_server": {
				Containers: []string{"prometheus"},
				Volumes:    []string{"prom-vol"},
				Services:   []string{"pi-service"},
			},
		},
		Features: map[string]AssignmentGroup{
			"mosquitto": {
				Kube:     []string{"mosquitto-stack"},
				Services: []string{"feature-service", "base-service"},
			},
		},
	}

	tests := []struct {
		name      string
		host      *HostConfig
		wantNets  int
		wantConts int
		wantKubes int
		wantVols  int
		wantSvcs  []string
	}{
		{
			name:      "server with mosquitto",
			host:      &HostConfig{PiType: "server", Features: []string{"mosquitto"}},
			wantNets:  1,
			wantConts: 1,
			wantKubes: 1,
			wantSvcs:  []string{"base-service", "feature-service"},
		},
		{
			name:      "monitoring_server no features",
			host:      &HostConfig{PiType: "monitoring_server"},
			wantNets:  1,
			wantConts: 2,
			wantVols:  1,
			wantSvcs:  []string{"base-service", "pi-service"},
		},
		{
			name:      "server no features",
			host:      &HostConfig{PiType: "server"},
			wantNets:  1,
			wantConts: 1,
			wantSvcs:  []string{"base-service"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := assignments.Resolve(tt.host)
			assert.Len(t, result.Networks, tt.wantNets)
			assert.Len(t, result.Containers, tt.wantConts)
			assert.Len(t, result.Kube, tt.wantKubes)
			assert.Len(t, result.Volumes, tt.wantVols)
			assert.Equal(t, tt.wantSvcs, result.Services)
		})
	}
}

func TestLoadAllMissingFleet(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{}
	_, err := LoadAll(fsys)
	require.Error(t, err)
}

func TestLoadAllRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\nnot_a_field: true\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte("base: {}\n")},
		"hosts/host-a/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-a
features: []
`)},
	}
	_, err := LoadAll(fsys)
	require.Error(t, err)
	require.ErrorContains(t, err, "not_a_field")
}

//nolint:funlen // table-driven mismatched-field coverage is clearer inline.
func TestSecretHookNormalizeRejectsMismatchedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		hook    Hook
		wantErr string
	}{
		{
			name: "http container",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionHTTP,
				URL:       "http://example.test/reload",
				Container: "app",
			},
			wantErr: "container cannot be set for http hooks",
		},
		{
			name: "http signal",
			hook: Hook{
				Name:    "hook",
				Secrets: []string{"cfg"},
				Unit:    "app.service",
				Action:  HookActionHTTP,
				URL:     "http://example.test/reload",
				Signal:  "HUP",
			},
			wantErr: "signal cannot be set for http hooks",
		},
		{
			name: "signal method",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionSignal,
				Method:    http.MethodPost,
				Container: "app",
			},
			wantErr: "method cannot be set for signal hooks",
		},
		{
			name: "signal url",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionSignal,
				URL:       "http://example.test/reload",
				Container: "app",
			},
			wantErr: "url cannot be set for signal hooks",
		},
		{
			name: "signal health url",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionSignal,
				HealthURL: "http://example.test/health",
				Container: "app",
			},
			wantErr: "health_url cannot be set for signal hooks",
		},
		{
			name: "restart health url",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionRestart,
				HealthURL: "http://example.test/health",
			},
			wantErr: "health_url cannot be set for restart hooks",
		},
		{
			name: "restart container",
			hook: Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionRestart,
				Container: "app",
			},
			wantErr: "container cannot be set for restart hooks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.hook.Normalize()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestSecretHookNormalizeValidatesURLScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		healthURL string
		wantErr   string
	}{
		{name: "http ok", url: "http://example.test/reload"},
		{name: "https ok", url: "https://example.test/reload"},
		{name: "http with path and port", url: "http://localhost:8080/-/reload"},
		{name: "https with health url", url: "https://example.test/reload", healthURL: "https://example.test/health"},
		{name: "no scheme", url: "example.test/reload", wantErr: "url must use http or https"},
		{name: "file scheme", url: "file:///etc/passwd", wantErr: "url must use http or https"},
		{name: "ftp scheme", url: "ftp://example.test", wantErr: "url must use http or https"},
		{name: "no host", url: "http:///path", wantErr: "url must include an explicit host"},
		{name: "scheme relative", url: "//example.test/path", wantErr: "url must use http or https"},
		{name: "invalid health url", url: "http://example.test/reload", healthURL: "file:///x", wantErr: "health_url must use http or https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := Hook{
				Name:      "hook",
				Secrets:   []string{"cfg"},
				Unit:      "app.service",
				Action:    HookActionHTTP,
				URL:       tt.url,
				HealthURL: tt.healthURL,
			}
			err := h.Normalize()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestHookNormalizeValidatesManifests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{name: "simple file", manifest: "scrape.yml"},
		{name: "nested file", manifest: "config/scrape.yml"},
		{name: "double dot in filename allowed", manifest: "foo..bar/baz.yml"},
		{name: "absolute path rejected", manifest: "/etc/passwd", wantErr: "must be a clean relative path"},
		{name: "traversal rejected", manifest: "../etc/passwd", wantErr: "must be a clean relative path"},
		{name: "embedded traversal rejected", manifest: "a/../b", wantErr: "must be a clean relative path"},
		{name: "double slash rejected", manifest: "a//b.yml", wantErr: "must be a clean relative path"},
		{name: "dot rejected", manifest: ".", wantErr: "must be a clean relative path"},
		{name: "empty rejected", manifest: "", wantErr: "must be a clean relative path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := Hook{
				Name:      "hook",
				Manifests: []string{tt.manifest},
				Unit:      "app.service",
				Action:    HookActionRestart,
			}
			err := h.Normalize()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
