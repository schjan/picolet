package validator

import (
	"path/filepath"
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// newParsedFile returns a ResolvedFile with ParsedUnit populated, for use in test fixtures.
func newParsedFile(t *testing.T, category config.Category, destPath, content string) resolver.ResolvedFile {
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

func TestAnalyzeFilesQuadletDependencies(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		newParsedFile(t, "network", "/etc/containers/systemd/internal.network", "[Network]\n"),
		newParsedFile(t, "container", "/etc/containers/systemd/web.container", `[Unit]
Requires=custom.service
After=internal.network

[Container]
Image=docker.io/library/nginx:latest
Network=internal.network
`),
	}

	depsByUnit, err := AnalyzeFiles(files, false)
	require.NoError(t, err)

	deps := depsByUnit["web.service"]
	require.Contains(t, deps.Requires, "custom.service")
	require.Contains(t, deps.After, "internal-network.service")
	require.Contains(t, deps.Wants, "network-online.target")
}

// TestAnalyzeFiles_NoDoubleServiceSuffix is a regression test: an earlier
// version of buildUnitInfo set ServiceName=base+".service", and Quadlet's
// generator then ran ServiceFileName() (which appends ".service" again),
// producing dependency targets like "internal-network.service.service".
// The fix in pkg/validator/quadlet.go strips the suffix; this test pins it.
func TestAnalyzeFiles_NoDoubleServiceSuffix(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		newParsedFile(t, "network", "/etc/containers/systemd/internal.network", "[Network]\n"),
		newParsedFile(t, "container", "/etc/containers/systemd/web.container", `[Container]
Image=docker.io/library/nginx:latest
Network=internal.network
`),
	}

	depsByUnit, err := AnalyzeFiles(files, false)
	require.NoError(t, err)

	for unit, deps := range depsByUnit {
		require.NotContains(t, unit, ".service.service",
			"unit name must not contain doubled .service suffix")
		for _, target := range deps.After {
			require.NotContains(t, target, ".service.service",
				"After dep target must not contain doubled .service suffix")
		}
		for _, target := range deps.Requires {
			require.NotContains(t, target, ".service.service",
				"Requires dep target must not contain doubled .service suffix")
		}
	}
}

func TestAnalyzeFilesRootlessDefaultDependencies(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		newParsedFile(t, "container", "/etc/containers/systemd/web.container", `[Container]
Image=docker.io/library/nginx:latest
`),
	}

	depsByUnit, err := AnalyzeFiles(files, true)
	require.NoError(t, err)

	deps := depsByUnit["web.service"]
	require.Contains(t, deps.After, "podman-user-wait-network-online.service")
	require.Contains(t, deps.Wants, "podman-user-wait-network-online.service")
}

func TestAnalyzeFilesSystemdDependencies(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{{
		DestPath: "/etc/systemd/system/custom.service",
		Category: "systemd",
		Content: `[Unit]
Requires=network-online.target
After=network-online.target

[Service]
ExecStart=/bin/true
`,
	}}

	depsByUnit, err := AnalyzeFiles(files, false)
	require.NoError(t, err)

	deps := depsByUnit["custom.service"]
	require.Equal(t, []string{"network-online.target"}, deps.Requires)
	require.Equal(t, []string{"network-online.target"}, deps.After)
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
			name: "invalid second document",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
---
invalid: [yaml: broken
`,
			wantErr: true,
			errMsg:  "document 2: YAML parse error",
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

func TestValidateSystemdUnitTimer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "valid OnCalendar timer",
			content: "[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n",
		},
		{
			name:    "valid OnUnitActiveSec timer",
			content: "[Timer]\nOnBootSec=5min\nOnUnitActiveSec=1h\n",
		},
		{
			name:    "timer without On* trigger fails",
			content: "[Timer]\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n",
			wantErr: true,
		},
		{
			name:    "empty timer section fails",
			content: "[Timer]\n",
			wantErr: true,
		},
		{
			// A [Timer] string appearing only in a value must not satisfy the check.
			name:    "timer mentioned in value is not a section",
			content: "[Service]\nEnvironment=NOTE=[Timer]OnCalendar=x\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSystemdUnit("test.timer", tt.content)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorContains(t, err, "On*=")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateFilesRejectsUnknownCategory(t *testing.T) {
	t.Parallel()
	err := ValidateFiles([]resolver.ResolvedFile{{
		DestPath: "/tmp/file",
		SrcPath:  "unknown/file",
		Category: "unknown",
		Content:  "content",
	}}, false)
	require.Error(t, err)
	require.ErrorContains(t, err, `unknown file category "unknown"`)
}

func TestSplitYAMLDocumentsLeadingSeparator(t *testing.T) {
	t.Parallel()
	content := []byte("---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: test\n")
	docs, err := splitYAMLDocuments(content)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Contains(t, string(docs[0]), "apiVersion: v1")
}

func TestSplitYAMLDocumentsMulti(t *testing.T) {
	t.Parallel()
	content := []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: b\n")
	docs, err := splitYAMLDocuments(content)
	require.NoError(t, err)
	require.Len(t, docs, 2)
}

func TestValidateYAMLSyntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr bool
		errMsg  string
	}{
		{
			name: "single document",
			content: `a:
  b: c
`,
		},
		{
			name: "multi document",
			content: `a: b
---
c: d
`,
		},
		{
			name:    "invalid second document",
			content: "a: b\n---\ninvalid: [yaml: broken\n",
			wantErr: true,
			errMsg:  "document 2: YAML parse error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateYAMLSyntax("secret:test", []byte(tt.content))
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

//nolint:funlen // table-driven validation test
func TestValidateSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    resolver.ResolvedFile
		wantErr bool
		errMsg  string
	}{
		{
			name: "empty secret",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/empty.yml",
				DestPath: "secret:empty",
				Category: "secret",
			},
			wantErr: true,
			errMsg:  "empty secret content",
		},
		{
			name: "repo yaml syntax error",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/bad.yml",
				DestPath: "secret:bad",
				Category: "secret",
				Content:  "invalid: [yaml: broken",
			},
			wantErr: true,
			errMsg:  "YAML parse error",
		},
		{
			name: "templated yaml syntax error",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/bad.yaml.tmpl",
				DestPath: "secret:bad_template",
				Category: "secret",
				Content:  "invalid: [yaml: broken",
			},
			wantErr: true,
			errMsg:  "YAML parse error",
		},
		{
			name: "placeholder host-only yaml is skipped",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/host_only.yml",
				DestPath: "secret:host_only",
				Category: "secret",
				Content:  "<secret>",
			},
		},
		{
			name: "non-yaml secret only checks non-empty",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/token.txt",
				DestPath: "secret:token",
				Category: "secret",
				Content:  "token=abc",
			},
		},
		{
			name: "op secret stays out of yaml validation",
			file: resolver.ResolvedFile{
				SrcPath:  "op://vault/item/field",
				DestPath: "secret:item_field",
				Category: "secret",
				Content:  "invalid: [yaml: broken",
			},
		},
		{
			name: "valid yaml secret passes",
			file: resolver.ResolvedFile{
				SrcPath:  "secrets/good.yml",
				DestPath: "secret:good",
				Category: "secret",
				Content:  "a:\n  b: c\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSecret(tt.file)
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

func TestValidateFileTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		srcPath string
		content string
		wantErr string
	}{
		{name: "plain text passes", srcPath: "files/notes.txt", content: "hello"},
		{name: "yml valid passes", srcPath: "files/scrape.yml", content: "scrape_configs:\n  - job_name: x\n"},
		{name: "yaml valid passes", srcPath: "files/scrape.yaml", content: "scrape_configs: []\n"},
		{name: "uppercase yml validates", srcPath: "files/Mixed.YML", content: "scrape_configs: []\n"},
		{name: "uppercase yaml tmpl validates", srcPath: "files/Mixed.YAML.TMPL", content: "scrape_configs: []\n"},
		{name: "yml invalid fails", srcPath: "files/bad.yml", content: "scrape_configs: [\n - broken\n", wantErr: "YAML parse error"},
		{name: "yml.tmpl validates rendered", srcPath: "files/x.yml.tmpl", content: ":\n  - bad\n", wantErr: "YAML parse error"},
		{name: "empty file passes", srcPath: "files/empty.yml", content: ""},
		{name: "whitespace-only yml passes", srcPath: "files/blank.yml", content: " \n\t\n"},
		{name: "non-yaml extension skips validation", srcPath: "files/raw.bin", content: "\x00\x01not yaml\x02"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := resolver.ResolvedFile{
				SrcPath:  tt.srcPath,
				DestPath: "/var/lib/picolet/" + tt.srcPath,
				Content:  tt.content,
				Category: config.CategoryFile,
			}
			_, err := AnalyzeFiles([]resolver.ResolvedFile{f}, false)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
