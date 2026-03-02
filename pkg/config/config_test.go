package config

import (
	"testing"
	"testing/fstest"
)

func TestLoadAll(t *testing.T) {
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
ansible_host: host-a.ts.net
pi_type: server
features:
  - mosquitto
`)},
		"hosts/host-b/host.yml": &fstest.MapFile{Data: []byte(`
hostname: host-b
ansible_host: host-b.ts.net
pi_type: monitoring_server
features: []
`)},
	}

	cfg, err := LoadAll(fsys)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Check fleet config
	if got := cfg.Fleet.Images["traefik"]; got != "traefik:v3" {
		t.Errorf("Fleet.Images[traefik] = %q, want traefik:v3", got)
	}
	if got := cfg.Fleet.Ports["alloy_http"]; got != 12345 {
		t.Errorf("Fleet.Ports[alloy_http] = %d, want 12345", got)
	}
	if got := cfg.Fleet.Prometheus.RetentionTime; got != "35d" {
		t.Errorf("Fleet.Prometheus.RetentionTime = %q, want 35d", got)
	}

	// Check hosts
	if len(cfg.Hosts) != 2 {
		t.Fatalf("len(Hosts) = %d, want 2", len(cfg.Hosts))
	}
	if got := cfg.Hosts["host-a"].PiType; got != "server" {
		t.Errorf("Hosts[host-a].PiType = %q, want server", got)
	}
	if got := cfg.Hosts["host-b"].PiType; got != "monitoring_server" {
		t.Errorf("Hosts[host-b].PiType = %q, want monitoring_server", got)
	}

	// Check sorted hostnames
	hostnames := cfg.SortedHostnames()
	if len(hostnames) != 2 || hostnames[0] != "host-a" || hostnames[1] != "host-b" {
		t.Errorf("SortedHostnames() = %v, want [host-a, host-b]", hostnames)
	}
}

func TestAssignmentsResolve(t *testing.T) {
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
			result := assignments.Resolve(tt.host)
			if got := len(result.Networks); got != tt.wantNets {
				t.Errorf("Networks count = %d, want %d", got, tt.wantNets)
			}
			if got := len(result.Containers); got != tt.wantConts {
				t.Errorf("Containers count = %d, want %d", got, tt.wantConts)
			}
			if got := len(result.Kube); got != tt.wantKubes {
				t.Errorf("Kube count = %d, want %d", got, tt.wantKubes)
			}
			if got := len(result.Volumes); got != tt.wantVols {
				t.Errorf("Volumes count = %d, want %d", got, tt.wantVols)
			}
		})
	}
}

func TestLoadAllMissingFleet(t *testing.T) {
	fsys := fstest.MapFS{}
	_, err := LoadAll(fsys)
	if err == nil {
		t.Fatal("expected error for missing fleet.yml")
	}
}
