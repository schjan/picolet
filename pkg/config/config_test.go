package config

import (
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

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
`)},
		"assignments.yml": &fstest.MapFile{Data: []byte(`
base:
  paths:
    - quadlets/networks/internal.network
    - quadlets/containers/traefik.container.tmpl
roles:
  monitoring_server:
    paths:
      - quadlets/containers/prometheus.container.tmpl
features:
  mosquitto:
    paths:
      - quadlets/kube/mosquitto-stack.kube.tmpl
`)},
		"hosts/host-a/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-a
external_hostname: host-a.ts.net
role: server
features:
  - mosquitto
`)},
		"hosts/host-b/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-b
external_hostname: host-b.ts.net
role: monitoring_server
features: []
`)},
	}

	cfg, err := LoadAll(fsys)
	require.NoError(t, err)

	// Check fleet config
	assert.Equal(t, "traefik:v3", cfg.Fleet.Images["traefik"])
	assert.Equal(t, 12345, cfg.Fleet.Ports["alloy_http"])
	assert.Equal(t, 9090, cfg.Fleet.Ports["prometheus"])

	// Check hosts
	require.Len(t, cfg.Hosts, 2)
	assert.Equal(t, "server", cfg.Hosts["host-a"].Role)
	assert.Equal(t, "monitoring_server", cfg.Hosts["host-b"].Role)
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
			Paths:    []string{"quadlets/net1.network", "quadlets/base.container"},
			Secrets:  []string{"secrets/base.yml"},
			Services: []string{"base-service"},
		},
		Roles: map[string]AssignmentGroup{
			"monitoring_server": {
				Paths:    []string{"quadlets/prometheus.container", "volumes/"},
				Services: []string{"pi-service"},
			},
		},
		Features: map[string]AssignmentGroup{
			"mosquitto": {
				Paths:    []string{"mosquitto/", "mosquitto/", "quadlets/base.container"},
				Secrets:  []string{"op://vault/mqtt/password", "secrets/base.yml"},
				Services: []string{"feature-service", "base-service"},
			},
		},
	}

	tests := []struct {
		name string
		host *HostConfig
		want *ResolvedFileSet
	}{
		{
			name: "server with mosquitto",
			host: &HostConfig{Role: "server", Features: []string{"mosquitto"}},
			want: &ResolvedFileSet{
				Paths:    []string{"mosquitto/", "quadlets/base.container", "quadlets/net1.network"},
				Secrets:  []string{"op://vault/mqtt/password", "secrets/base.yml"},
				Services: []string{"base-service", "feature-service"},
			},
		},
		{
			name: "monitoring_server no features",
			host: &HostConfig{Role: "monitoring_server"},
			want: &ResolvedFileSet{
				Paths:    []string{"quadlets/base.container", "quadlets/net1.network", "quadlets/prometheus.container", "volumes/"},
				Secrets:  []string{"secrets/base.yml"},
				Services: []string{"base-service", "pi-service"},
			},
		},
		{
			name: "server no features",
			host: &HostConfig{Role: "server"},
			want: &ResolvedFileSet{
				Paths:    []string{"quadlets/base.container", "quadlets/net1.network"},
				Secrets:  []string{"secrets/base.yml"},
				Services: []string{"base-service"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, assignments.Resolve(tt.host))
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

// TestValidateRejectsRetiredKeysExactly pins the migration text at the
// validation boundary, where it is unwrapped. Every value variant must be
// rejected: presence of the retired key is what matters, not what it carried.
func TestValidateRejectsRetiredKeysExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		newCfg  func() interface{ Validate() error }
		wantErr string
		// docs are value variants of the same retired key; presence alone must
		// trigger the rejection, so an empty or null value counts too.
		docs []string
	}{
		{
			name:    "host.yml pi_type",
			newCfg:  func() interface{ Validate() error } { return &HostConfig{} },
			wantErr: "host.yml: 'pi_type:' was renamed to 'role:'",
			docs: []string{
				"hostname: host-a\npi_type: server\n",
				"hostname: host-a\nrole: server\npi_type: \"\"\n",
				"hostname: host-a\nrole: server\npi_type:\n",
			},
		},
		{
			name:    "assignments.yml pi_types",
			newCfg:  func() interface{ Validate() error } { return &Assignments{} },
			wantErr: "assignments.yml: 'pi_types:' was renamed to 'roles:'",
			docs: []string{
				"base: {}\npi_types:\n  server: {}\n",
				"base: {}\npi_types: {}\n",
				"base: {}\npi_types:\n",
			},
		},
		{
			name:    "fleet.yml prometheus",
			newCfg:  func() interface{ Validate() error } { return &FleetConfig{} },
			wantErr: "fleet.yml: 'prometheus:' was removed from the schema; delete it",
			docs: []string{
				"images: {}\nprometheus:\n  retention_time: \"35d\"\n",
				"images: {}\nprometheus: {}\n",
				"images: {}\nprometheus:\n",
			},
		},
	}

	for _, tt := range tests {
		for _, doc := range tt.docs {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				cfg := tt.newCfg()
				require.NoError(t, yaml.Load([]byte(doc), cfg, yaml.WithKnownFields()))
				require.EqualError(t, cfg.Validate(), tt.wantErr, "document: %q", doc)
			})
		}
	}
}

// Every retired typed list is rejected in every group, naming the group, the
// key and `paths:`; presence alone counts.
func TestValidateRejectsTypedLists(t *testing.T) {
	t.Parallel()
	keys := []string{"networks", "systemd", "volumes", "containers", "kube", "pods", "images", "builds", "manifests", "files"}
	groups := []struct {
		name string
		doc  func(entry string) string
	}{
		{"base", func(entry string) string { return "base:\n  " + entry + "\n" }},
		{"roles.worker", func(entry string) string { return "roles:\n  worker:\n    " + entry + "\n" }},
		{"features.obs", func(entry string) string { return "features:\n  obs:\n    " + entry + "\n" }},
	}
	for _, key := range keys {
		for _, g := range groups {
			for variant, value := range map[string]string{"list": " [quadlets/a.container]", "empty": " []", "null": ""} {
				doc := g.doc(key + ":" + value)
				t.Run(g.name+"/"+key+"/"+variant, func(t *testing.T) {
					t.Parallel()
					var a Assignments
					require.NoError(t, yaml.Load([]byte(doc), &a, yaml.WithKnownFields()))
					require.EqualError(t, a.Validate(),
						"assignments.yml: "+g.name+": '"+key+":' was removed; list these files under 'paths:', which derives the category from the file name",
						"document: %q", doc)
				})
			}
		}
	}
}

func TestValidateReportsEveryTypedList(t *testing.T) {
	t.Parallel()
	doc := "features:\n  obs:\n    files: []\nroles:\n  worker:\n    kube: []\n    containers: []\nbase:\n  paths: [a/]\n  secrets: [s]\n  services: [x]\n"
	var a Assignments
	require.NoError(t, yaml.Load([]byte(doc), &a, yaml.WithKnownFields()))
	const tail = "' was removed; list these files under 'paths:', which derives the category from the file name"
	require.EqualError(t, a.Validate(), "assignments.yml: roles.worker: 'containers:"+tail+"\n"+
		"assignments.yml: roles.worker: 'kube:"+tail+"\n"+
		"assignments.yml: features.obs: 'files:"+tail)
}

// TestLoadAllRejectsRetiredKeys covers the wiring: each Validate is reached
// from LoadAll, whichever file carries the retired key.
func TestLoadAllRejectsRetiredKeys(t *testing.T) {
	t.Parallel()

	const (
		goodFleet       = "images: {}\nports: {}\n"
		goodAssignments = "base: {}\nroles: {}\nfeatures: {}\n"
		goodHost        = "hostname: host-a\nrole: server\nfeatures: []\n"
	)

	tests := []struct {
		name        string
		fleet       string
		assignments string
		host        string
		wantMessage string
	}{
		{
			name:        "pi_type in host.yml",
			fleet:       goodFleet,
			assignments: goodAssignments,
			host:        "hostname: host-a\npi_type: server\nfeatures: []\n",
			wantMessage: migratePiType,
		},
		{
			name:        "pi_types in assignments.yml",
			fleet:       goodFleet,
			assignments: "base: {}\npi_types: {}\nfeatures: {}\n",
			host:        goodHost,
			wantMessage: migratePiTypes,
		},
		{
			name:        "prometheus in fleet.yml",
			fleet:       "images: {}\nports: {}\nprometheus:\n  retention_time: \"35d\"\n",
			assignments: goodAssignments,
			host:        goodHost,
			wantMessage: migratePrometheus,
		},
		{
			name:        "typed list in assignments.yml",
			fleet:       goodFleet,
			assignments: "base: {}\nroles:\n  worker:\n    containers: [quadlets/a.container]\nfeatures: {}\n",
			host:        goodHost,
			wantMessage: "assignments.yml: roles.worker: 'containers:' was removed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				"fleet.yml":             &fstest.MapFile{Data: []byte(tt.fleet)},
				"assignments.yml":       &fstest.MapFile{Data: []byte(tt.assignments)},
				"hosts/host-a/host.yml": &fstest.MapFile{Data: []byte(tt.host)},
			}
			_, err := LoadAll(fsys)
			require.ErrorContains(t, err, tt.wantMessage)
		})
	}
}

// A ports entry named "prometheus" is a port name, not the retired key.
func TestLoadAllAcceptsPortNamedPrometheus(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"fleet.yml":             &fstest.MapFile{Data: []byte("images: {}\nports:\n  prometheus: 9090\n")},
		"assignments.yml":       &fstest.MapFile{Data: []byte("base: {}\nroles: {}\nfeatures: {}\n")},
		"hosts/host-a/host.yml": &fstest.MapFile{Data: []byte("hostname: host-a\nrole: server\nfeatures: []\n")},
	}
	cfg, err := LoadAll(fsys)
	require.NoError(t, err)
	assert.Equal(t, 9090, cfg.Fleet.Ports["prometheus"])
}

func TestHostConfigValidateRequiresRole(t *testing.T) {
	t.Parallel()
	require.EqualError(t, (&HostConfig{Hostname: "host-a"}).Validate(), "role is required")
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

func TestHookNormalizeValidatesFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{name: "simple file", file: "scrape.yml"},
		{name: "nested file", file: "config/scrape.yml"},
		{name: "absolute path rejected", file: "/etc/passwd", wantErr: "must be a clean relative path"},
		{name: "traversal rejected", file: "../etc/passwd", wantErr: "must be a clean relative path"},
		{name: "embedded traversal rejected", file: "a/../b", wantErr: "must be a clean relative path"},
		{name: "double slash rejected", file: "a//b.yml", wantErr: "must be a clean relative path"},
		{name: "dot rejected", file: ".", wantErr: "must be a clean relative path"},
		{name: "empty rejected", file: "", wantErr: "must be a clean relative path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := Hook{
				Name:   "hook",
				Files:  []string{tt.file},
				Unit:   "app.service",
				Action: HookActionRestart,
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

func TestHookNormalizeRequiresAtLeastOneTrigger(t *testing.T) {
	t.Parallel()
	h := Hook{Name: "hook", Unit: "app.service", Action: HookActionRestart}
	err := h.Normalize()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of secrets, manifests, or files is required")
}

func TestHookNormalizeFilesOnlyTrigger(t *testing.T) {
	t.Parallel()
	h := Hook{
		Name:   "hook",
		Files:  []string{"config/foo.yml"},
		Unit:   "app.service",
		Action: HookActionRestart,
	}
	require.NoError(t, h.Normalize())
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
