package config

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen,cyclop // table-driven test
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

func TestAssignmentsResolve(t *testing.T) {
	t.Parallel()
	assignments := &Assignments{
		Base: AssignmentGroup{
			Networks:   []string{"net1"},
			Containers: []string{"base-container"},
		},
		PiTypes: map[string]AssignmentGroup{
			"monitoring_server": {
				Containers: []string{"prometheus"},
				Volumes:    []string{"prom-vol"},
			},
		},
		Features: map[string]AssignmentGroup{
			"mosquitto": {
				Kube: []string{"mosquitto-stack"},
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
	}{
		{
			name:      "server with mosquitto",
			host:      &HostConfig{PiType: "server", Features: []string{"mosquitto"}},
			wantNets:  1,
			wantConts: 1,
			wantKubes: 1,
		},
		{
			name:      "monitoring_server no features",
			host:      &HostConfig{PiType: "monitoring_server"},
			wantNets:  1,
			wantConts: 2,
			wantVols:  1,
		},
		{
			name:      "server no features",
			host:      &HostConfig{PiType: "server"},
			wantNets:  1,
			wantConts: 1,
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
		})
	}
}

func TestLoadAllMissingFleet(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{}
	_, err := LoadAll(fsys)
	require.Error(t, err)
}
