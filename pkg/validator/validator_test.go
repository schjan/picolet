package validator

import (
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/resolver"
)

//nolint:funlen // table-driven validation test
func TestValidateQuadletContainer(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			v := New()
			unitsInfo := buildUnitsInfoFromFiles(tt.files)
			err := v.validateQuadlet(tt.path, []byte(tt.content), unitsInfo)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					require.ErrorContains(t, err, tt.errMsg)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateQuadletKube(t *testing.T) {
	t.Parallel()
	v := New()

	valid := `[Kube]
Yaml=/var/lib/picolet/manifests/alloy/deployment.yml
Network=internal.network

[Install]
WantedBy=default.target
`
	files := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/internal.network", Category: "network"},
	}
	unitsInfo := buildUnitsInfoFromFiles(files)
	require.NoError(t, v.validateQuadlet("/etc/containers/systemd/test.kube", []byte(valid), unitsInfo))

	v2 := New()
	noYaml := "[Kube]\nNetwork=internal.network\n"
	require.Error(t, v2.validateQuadlet("/etc/containers/systemd/test.kube", []byte(noYaml), make(map[string]*quadlet.UnitInfo)))
}

func TestValidateQuadletNetwork(t *testing.T) {
	t.Parallel()
	v := New()
	valid := "[Network]\nInternal=true\n"
	require.NoError(t, v.validateQuadlet("/etc/containers/systemd/test.network", []byte(valid), make(map[string]*quadlet.UnitInfo)))
}

func TestValidateQuadletVolume(t *testing.T) {
	t.Parallel()
	v := New()
	valid := "[Volume]\n"
	require.NoError(t, v.validateQuadlet("/etc/containers/systemd/test.volume", []byte(valid), make(map[string]*quadlet.UnitInfo)))
}

//nolint:funlen // table-driven validation test
func TestValidateManifest(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			err := v.validateManifest("test.yml", []byte(tt.content))
			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					require.ErrorContains(t, err, tt.errMsg)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateSystemdUnit(t *testing.T) {
	t.Parallel()
	v := New()

	valid := "[Socket]\nListenStream=80\n"
	require.NoError(t, v.validateSystemdUnit("test.socket", valid))

	empty := ""
	require.Error(t, v.validateSystemdUnit("test.socket", empty))

	noSection := "ListenStream=80"
	require.Error(t, v.validateSystemdUnit("test.socket", noSection))
}

func TestSplitYAMLDocumentsLeadingSeparator(t *testing.T) {
	t.Parallel()
	content := []byte("---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: test\n")
	docs := splitYAMLDocuments(content)
	require.Len(t, docs, 1)
	require.Contains(t, string(docs[0]), "apiVersion: v1")
}

func TestSplitYAMLDocumentsMulti(t *testing.T) {
	t.Parallel()
	content := []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: b\n")
	docs := splitYAMLDocuments(content)
	require.Len(t, docs, 2)
}
