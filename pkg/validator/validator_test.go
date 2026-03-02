package validator

import (
	"strings"
	"testing"

	"github.com/schjan/picolet/pkg/resolver"
)

func TestValidateQuadletContainer(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		files   []resolver.ResolvedFile // pre-populate unitsInfoMap
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid container with network ref",
			path: "/etc/containers/systemd/test.container",
			content: `[Container]
Image=docker.io/traefik:v3
Network=internal.network

[Install]
WantedBy=default.target
`,
			files: []resolver.ResolvedFile{
				{DestPath: "/etc/containers/systemd/internal.network", Category: "network"},
			},
		},
		{
			name: "valid minimal container",
			path: "/etc/containers/systemd/test.container",
			content: `[Container]
Image=docker.io/traefik:v3
`,
		},
		{
			name:    "missing Image key",
			path:    "/etc/containers/systemd/test.container",
			content: "[Container]\n",
			wantErr: true,
		},
		{
			name:    "empty content",
			path:    "/etc/containers/systemd/test.container",
			content: "",
			wantErr: true,
		},
		{
			name: "unresolved network reference",
			path: "/etc/containers/systemd/test.container",
			content: `[Container]
Image=docker.io/traefik:v3
Network=nonexistent.network
`,
			wantErr: true,
			errMsg:  "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New()
			v.currentFiles = tt.files
			err := v.validateQuadlet(tt.path, []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.errMsg)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateQuadletKube(t *testing.T) {
	v := New()

	valid := `[Kube]
Yaml=/var/lib/picolet/manifests/alloy/deployment.yml
Network=internal.network

[Install]
WantedBy=default.target
`
	v.currentFiles = []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/internal.network", Category: "network"},
	}
	if err := v.validateQuadlet("/etc/containers/systemd/test.kube", []byte(valid)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v2 := New()
	noYaml := "[Kube]\nNetwork=internal.network\n"
	if err := v2.validateQuadlet("/etc/containers/systemd/test.kube", []byte(noYaml)); err == nil {
		t.Fatal("expected error for missing Yaml key")
	}
}

func TestValidateQuadletNetwork(t *testing.T) {
	v := New()

	valid := "[Network]\nInternal=true\n"
	if err := v.validateQuadlet("/etc/containers/systemd/test.network", []byte(valid)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateQuadletVolume(t *testing.T) {
	v := New()

	valid := "[Volume]\n"
	if err := v.validateQuadlet("/etc/containers/systemd/test.volume", []byte(valid)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateManifest(t *testing.T) {
	v := New()

	tests := []struct {
		name    string
		content string
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid Deployment",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
spec:
  replicas: 1
`,
		},
		{
			name: "valid multi-document ConfigMap + Deployment",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  key: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
`,
		},
		{
			name:    "unsupported kind",
			content: "apiVersion: v1\nkind: Service\nmetadata:\n  name: test\n",
			wantErr: true,
			errMsg:  "not supported by podman kube play",
		},
		{
			name:    "missing kind",
			content: "apiVersion: v1\nmetadata:\n  name: test\n",
			wantErr: true,
			errMsg:  "missing 'kind'",
		},
		{
			name:    "missing metadata.name",
			content: "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
			wantErr: true,
			errMsg:  "missing 'metadata.name'",
		},
		{
			name:    "invalid YAML",
			content: "invalid: [yaml: broken",
			wantErr: true,
			errMsg:  "YAML parse error",
		},
		{
			name: "unknown field in Deployment (strict)",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
spec:
  notARealField: true
`,
			wantErr: true,
			errMsg:  "unknown field",
		},
		{
			name: "wrong type for replicas",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
spec:
  replicas: "not-a-number"
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.validateManifest("test.yml", []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.errMsg)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateSystemdUnit(t *testing.T) {
	v := New()

	valid := "[Socket]\nListenStream=80\n"
	if err := v.validateSystemdUnit("test.socket", valid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	empty := ""
	if err := v.validateSystemdUnit("test.socket", empty); err == nil {
		t.Fatal("expected error for empty unit")
	}

	noSection := "ListenStream=80"
	if err := v.validateSystemdUnit("test.socket", noSection); err == nil {
		t.Fatal("expected error for missing section header")
	}
}
