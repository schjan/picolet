package resolver

import (
	"strings"
	"testing"
	"testing/fstest"

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
ansible_host: test-host.ts.net
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

//nolint:cyclop // integration test with many assertions
func TestResolveHost(t *testing.T) {
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	r := New(fsys, cfg)
	resolved, err := r.ResolveHost("test-host")
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if resolved.Hostname != "test-host" {
		t.Errorf("Hostname = %q, want test-host", resolved.Hostname)
	}

	if len(resolved.Files) != 3 {
		t.Fatalf("len(Files) = %d, want 3", len(resolved.Files))
	}

	// Check network file (static)
	net := resolved.Files[0]
	if net.Category != "network" {
		t.Errorf("Files[0].Category = %q, want network", net.Category)
	}
	if !strings.Contains(net.Content, "Internal=true") {
		t.Errorf("network content missing Internal=true")
	}

	// Check container file (templated)
	cont := resolved.Files[1]
	if cont.Category != "container" {
		t.Errorf("Files[1].Category = %q, want container", cont.Category)
	}
	if !strings.Contains(cont.Content, "Image=traefik:v3.6.9") {
		t.Errorf("container content missing rendered image, got:\n%s", cont.Content)
	}
	if cont.DestPath != "/etc/containers/systemd/test.container" {
		t.Errorf("container DestPath = %q, want /etc/containers/systemd/test.container", cont.DestPath)
	}

	// Check manifest (templated)
	manifest := resolved.Files[2]
	if manifest.Category != "manifest" {
		t.Errorf("Files[2].Category = %q, want manifest", manifest.Category)
	}
	if !strings.Contains(manifest.Content, "image: \"traefik:v3.6.9\"") {
		t.Errorf("manifest content missing rendered image, got:\n%s", manifest.Content)
	}
	if !strings.Contains(manifest.Content, "containerPort: 12345") {
		t.Errorf("manifest content missing rendered port, got:\n%s", manifest.Content)
	}
}

func TestResolveHostNotFound(t *testing.T) {
	fsys := newTestFS()
	cfg, err := config.LoadAll(fsys)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	r := New(fsys, cfg)
	_, err = r.ResolveHost("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent host")
	}
}

func TestTemplateDataFields(t *testing.T) {
	cfg := &config.Config{
		Fleet: &config.FleetConfig{
			Images: map[string]string{"test": "img:v1"},
			Ports:  map[string]int{"test": 8080},
		},
		Hosts: map[string]*config.HostConfig{
			"server-host": {
				Hostname:    "server-host",
				AnsibleHost: "server.ts.net",
				PiType:      "server",
				Features:    []string{"mosquitto"},
			},
			"gateway-host": {
				Hostname:    "gateway-host",
				AnsibleHost: "gateway.ts.net",
				PiType:      "monitoring_server",
			},
		},
	}

	t.Run("server host", func(t *testing.T) {
		data, err := NewTemplateData(cfg, "server-host")
		if err != nil {
			t.Fatalf("NewTemplateData: %v", err)
		}
		if data.Host.AlloyMode != "agent" {
			t.Errorf("AlloyMode = %q, want agent", data.Host.AlloyMode)
		}
		if data.Host.IsGateway {
			t.Error("IsGateway = true, want false")
		}
		if !data.Host.HasMosquitto {
			t.Error("HasMosquitto = false, want true")
		}
		if len(data.Fleet.Hosts) != 2 {
			t.Errorf("len(Fleet.Hosts) = %d, want 2", len(data.Fleet.Hosts))
		}
	})

	t.Run("gateway host", func(t *testing.T) {
		data, err := NewTemplateData(cfg, "gateway-host")
		if err != nil {
			t.Fatalf("NewTemplateData: %v", err)
		}
		if data.Host.AlloyMode != "gateway" {
			t.Errorf("AlloyMode = %q, want gateway", data.Host.AlloyMode)
		}
		if !data.Host.IsGateway {
			t.Error("IsGateway = false, want true")
		}
		if data.Host.HasMosquitto {
			t.Error("HasMosquitto = true, want false")
		}
	})
}
