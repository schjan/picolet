package validator

import (
	"path/filepath"
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/resolver"
)

// newParsedFile returns a ResolvedFile with ParsedUnit populated, for use in test fixtures.
func newParsedFile(t *testing.T, category, destPath, content string) resolver.ResolvedFile {
	t.Helper()
	unit := parser.NewUnitFile()
	unit.Filename = filepath.Base(destPath)
	require.NoError(t, unit.Parse(content))
	return resolver.ResolvedFile{
		DestPath:   destPath,
		Category:   category,
		Content:    content,
		ParsedUnit: unit,
	}
}

// parseUnit parses a quadlet unit for use in validateQuadlet tests.
func parseUnit(t *testing.T, filename, content string) *parser.UnitFile {
	t.Helper()
	unit := parser.NewUnitFile()
	unit.Filename = filename
	require.NoError(t, unit.Parse(content))
	return unit
}

//nolint:funlen // table-driven validation test
func TestValidateQuadletContainer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		files   []resolver.ResolvedFile // pre-populate unitsInfoMap
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid container with network ref",
			content: `[Container]
Image=docker.io/traefik:v3
Network=internal.network

[Install]
WantedBy=default.target
`,
			files: []resolver.ResolvedFile{
				newParsedFile(t, "network", "/etc/containers/systemd/internal.network", "[Network]\n"),
			},
		},
		{
			name: "valid minimal container",
			content: `[Container]
Image=docker.io/traefik:v3
`,
		},
		{
			name:    "missing Image key",
			content: "[Container]\n",
			wantErr: true,
		},
		{
			name: "unresolved network reference",
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
			unitsInfo := buildUnitsInfoFromFiles(tt.files, false)

			unit := parser.NewUnitFile()
			unit.Filename = "test.container"
			require.NoError(t, unit.Parse(tt.content))

			err := validateQuadlet(unit, unitsInfo, false)
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

	valid := `[Kube]
Yaml=/var/lib/picolet/manifests/alloy/deployment.yml
Network=internal.network

[Install]
WantedBy=default.target
`
	files := []resolver.ResolvedFile{
		newParsedFile(t, "network", "/etc/containers/systemd/internal.network", "[Network]\n"),
	}
	unitsInfo := buildUnitsInfoFromFiles(files, false)
	unit := parseUnit(t, "test.kube", valid)
	require.NoError(t, validateQuadlet(unit, unitsInfo, false))

	noYaml := "[Kube]\nNetwork=internal.network\n"
	unit2 := parseUnit(t, "test.kube", noYaml)
	require.Error(t, validateQuadlet(unit2, make(map[string]*quadlet.UnitInfo), false))
}

func TestValidateQuadletNetwork(t *testing.T) {
	t.Parallel()
	unit := parseUnit(t, "test.network", "[Network]\nInternal=true\n")
	require.NoError(t, validateQuadlet(unit, make(map[string]*quadlet.UnitInfo), false))
}

func TestValidateQuadletVolume(t *testing.T) {
	t.Parallel()
	unit := parseUnit(t, "test.volume", "[Volume]\n")
	require.NoError(t, validateQuadlet(unit, make(map[string]*quadlet.UnitInfo), false))
}

//nolint:funlen // table-driven validation test
func TestValidateManifest(t *testing.T) {
	t.Parallel()

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
			err := validateManifest("test.yml", []byte(tt.content))
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

	valid := "[Socket]\nListenStream=80\n"
	require.NoError(t, validateSystemdUnit("test.socket", valid))

	empty := ""
	require.Error(t, validateSystemdUnit("test.socket", empty))

	noSection := "ListenStream=80"
	require.Error(t, validateSystemdUnit("test.socket", noSection))
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
