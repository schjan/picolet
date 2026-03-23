package config

import (
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

func TestDeduplicateAggregateSecrets(t *testing.T) {
	t.Parallel()

	t.Run("identical name+glob collapses to one", func(t *testing.T) {
		t.Parallel()
		entries := []AggregateSecret{
			{Name: "rules", Glob: "rules/*.yml", Header: "groups:\n"},
			{Name: "rules", Glob: "rules/*.yml", Header: ""},
		}
		result := deduplicateAggregateSecrets(entries)
		require.Len(t, result, 1)
		assert.Equal(t, "rules", result[0].Name)
		assert.Equal(t, "rules/*.yml", result[0].Glob)
	})

	t.Run("same name different glob keeps both", func(t *testing.T) {
		t.Parallel()
		entries := []AggregateSecret{
			{Name: "rules", Glob: "rules/common/*.yml"},
			{Name: "rules", Glob: "rules/monitoring/*.yml"},
		}
		result := deduplicateAggregateSecrets(entries)
		require.Len(t, result, 2)
		assert.Equal(t, "rules/common/*.yml", result[0].Glob)
		assert.Equal(t, "rules/monitoring/*.yml", result[1].Glob)
	})

	t.Run("different names kept and sorted", func(t *testing.T) {
		t.Parallel()
		entries := []AggregateSecret{
			{Name: "z-rules", Glob: "z/*.yml"},
			{Name: "a-rules", Glob: "a/*.yml"},
		}
		result := deduplicateAggregateSecrets(entries)
		require.Len(t, result, 2)
		assert.Equal(t, "a-rules", result[0].Name)
		assert.Equal(t, "z-rules", result[1].Name)
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		t.Parallel()
		result := deduplicateAggregateSecrets(nil)
		assert.Empty(t, result)
	})
}

func TestAssignmentsResolveAggregateSecrets(t *testing.T) {
	t.Parallel()
	assignments := &Assignments{
		Base: AssignmentGroup{
			AggregateSecrets: []AggregateSecret{
				{Name: "prometheus_rules", Glob: "rules/common/*.yml", Header: "groups:\n"},
			},
		},
		PiTypes: map[string]AssignmentGroup{},
		Features: map[string]AssignmentGroup{
			"monitoring": {
				AggregateSecrets: []AggregateSecret{
					// Same name, different glob: should be preserved (additive merge)
					{Name: "prometheus_rules", Glob: "rules/monitoring/*.yml"},
					// Duplicate of the base entry: should be deduplicated
					{Name: "prometheus_rules", Glob: "rules/common/*.yml"},
				},
			},
		},
	}

	host := &HostConfig{PiType: "server", Features: []string{"monitoring"}}
	result := assignments.Resolve(host)

	// Two unique (name, glob) pairs survive dedup
	require.Len(t, result.AggregateSecrets, 2)
	assert.Equal(t, "prometheus_rules", result.AggregateSecrets[0].Name)
	assert.Equal(t, "rules/common/*.yml", result.AggregateSecrets[0].Glob)
	assert.Equal(t, "prometheus_rules", result.AggregateSecrets[1].Name)
	assert.Equal(t, "rules/monitoring/*.yml", result.AggregateSecrets[1].Glob)
}

func TestLoadAllMissingFleet(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{}
	_, err := LoadAll(fsys)
	require.Error(t, err)
}
