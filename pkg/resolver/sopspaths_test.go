package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

// sopsCorpusFile reads a file of the shared SOPS fixture corpus.
func sopsCorpusFile(t *testing.T, name string) string {
	t.Helper()
	return sopsCorpusText(t, filepath.Join("corpus", name))
}

// sopsCorpusText reads a file of testdata/sops by its relative path.
func sopsCorpusText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sops", name))
	require.NoError(t, err)
	return string(data)
}

func TestResolveHostBundleSkipsSOPSFiles(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  services: [web]\n", bundleFiles("web", map[string]string{
		"web.container":     pathsUnit,
		"secrets.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
		"app.sops.env":      sopsCorpusFile(t, "values.sops.env"),
		"nested/.sops.yaml": "creation_rules: []\n",
		".sops.yml":         "creation_rules: []\n",
	}))
	require.NoError(t, err)
	assert.Equal(t, []pathsDeployment{
		{SrcPath: "services/web/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostBundleOfOnlySOPSFilesIsEmpty(t *testing.T) {
	t.Parallel()
	_, err := resolvePaths(t, "  services: [web]\n", bundleFiles("web", map[string]string{
		"secrets.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
	}))
	require.ErrorContains(t, err, "services/web: empty service bundle")
}

func TestResolveHostPathsDirectorySkipsSOPSFiles(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths: [secrets/, files/]\n", map[string]string{
		"secrets/api.yml":      "token: x\n",
		"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/.sops.yaml":   "creation_rules: []\n",
		"files/tls.sops.pem":   sopsCorpusFile(t, "tls.sops.pem"),
		"files/.sops.yaml":     "creation_rules: []\n",
		"files/sops-notes.md":  "notes\n",
		"files/app.sops":       "not a SOPS path: nothing follows the infix\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "secrets/api.yml", DestPath: "secret:api", Category: config.CategorySecret},
		{SrcPath: "files/sops-notes.md", DestPath: "/var/lib/picolet/files/sops-notes.md", Category: config.CategoryFile, RelPath: "sops-notes.md"},
		{SrcPath: "files/app.sops", DestPath: "/var/lib/picolet/files/app.sops", Category: config.CategoryFile, RelPath: "app.sops"},
	}, pathsDeployments(resolved.Files))
}

// A base name is a SOPS path only as <name>.sops.<ext>: `.sops.` anywhere
// else (a backup, a second extension) leaves an ordinary file the walk
// deploys like any other.
func TestResolveHostPathsDeploysFilesWithSOPSInfixElsewhere(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths: [files/]\n", map[string]string{
		"files/db.sops.yaml":      sopsCorpusFile(t, "values.sops.yaml"),
		"files/db.sops.yaml.bak":  "backup\n",
		"files/db.sops.prod.yaml": "prod: true\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "files/db.sops.yaml.bak", DestPath: "/var/lib/picolet/files/db.sops.yaml.bak", Category: config.CategoryFile, RelPath: "db.sops.yaml.bak"},
		{SrcPath: "files/db.sops.prod.yaml", DestPath: "/var/lib/picolet/files/db.sops.prod.yaml", Category: config.CategoryFile, RelPath: "db.sops.prod.yaml"},
	}, pathsDeployments(resolved.Files))
}

// A file that merely mentions sops keeps today's rules: outside files/ an
// unknown extension is still an error.
func TestResolveHostPathsSOPSLookalikeKeepsExtensionRule(t *testing.T) {
	t.Parallel()
	_, err := resolvePaths(t, "  paths: [docs/]\n", map[string]string{
		"docs/sops-notes.md": "notes\n",
	})
	require.ErrorContains(t, err, `docs/sops-notes.md: unknown extension ".md"`)
}

func TestResolveHostRejectsExplicitSOPSEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "paths entry",
			base: "  paths: [secrets/db.sops.yaml]\n",
			want: `paths entry "secrets/db.sops.yaml": SOPS file is reachable only through a sops:// ref (sops://secrets/db.sops.yaml#/...)`,
		},
		{
			name: "paths entry inside a bundle",
			base: "  paths: [services/web/app.sops.env]\n",
			want: `paths entry "services/web/app.sops.env": SOPS file is reachable only through a sops:// ref`,
		},
		{
			name: "paths entry naming .sops.yaml",
			base: "  paths: [files/.sops.yaml]\n",
			want: `paths entry "files/.sops.yaml": SOPS config file is never deployed`,
		},
		{
			name: "paths entry naming .sops.yml",
			base: "  paths: [.sops.yml]\n",
			want: `paths entry ".sops.yml": SOPS config file is never deployed`,
		},
		{
			name: "plain secrets entry",
			base: "  secrets: [secrets/db.sops.yaml]\n",
			want: `secrets entry "secrets/db.sops.yaml": SOPS file is reachable only through a sops:// ref (sops://secrets/db.sops.yaml#/...)`,
		},
		{
			name: "plain secrets entry without a directory",
			base: "  secrets: [db.sops.yaml]\n",
			want: `secrets entry "db.sops.yaml": SOPS file is reachable only through a sops:// ref`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, tt.base, map[string]string{
				"secrets/db.sops.yaml":      sopsCorpusFile(t, "values.sops.yaml"),
				"services/web/app.sops.env": sopsCorpusFile(t, "values.sops.env"),
				"files/.sops.yaml":          "creation_rules: []\n",
				".sops.yml":                 "creation_rules: []\n",
			})
			require.ErrorContains(t, err, tt.want)
		})
	}
}
