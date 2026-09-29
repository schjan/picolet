package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Fleet repo must not read host files outside itself through symlinks: the
// agent runs as root and deploys what the repo resolves to.
func TestLoadAndResolveHostRejectsSymlinksEscapingTheRepo(t *testing.T) {
	t.Parallel()
	const secret = "HOST-SECRET-OUTSIDE-REPO"
	tests := []struct {
		name  string
		base  string
		files map[string]string
		links map[string]string // repo path -> target relative to the outside dir
	}{
		{
			name:  "paths file entry naming a symlink",
			base:  "  paths: [files/leak.conf]\n",
			links: map[string]string{"files/leak.conf": "leak.conf"},
		},
		{
			name:  "paths entry naming a directory symlink",
			base:  "  paths: [files/outside]\n",
			links: map[string]string{"files/outside": "."},
		},
		{
			name:  "template readFile",
			base:  "  paths: [quadlets/app.container.tmpl]\n",
			files: map[string]string{"quadlets/app.container.tmpl": "[Container]\nImage=x\nEnvironment=X={{ readFile \"files/leak.conf\" }}\n"},
			links: map[string]string{"files/leak.conf": "leak.conf"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outside := t.TempDir()
			writeTestFile(t, outside, "leak.conf", secret)
			repo := t.TempDir()
			writeTestFile(t, repo, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
			writeTestFile(t, repo, "assignments.yml", "base:\n"+tt.base+"roles: {}\nfeatures: {}\n")
			writeTestFile(t, repo, "hosts/h1/host.yml", "hostname: h1\nexternal_hostname: h1.example.net\nrole: node\nfeatures: []\n")
			for p, content := range tt.files {
				writeTestFile(t, repo, p, content)
			}
			for link, target := range tt.links {
				full := filepath.Join(repo, link)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.Symlink(filepath.Join(outside, target), full))
			}

			resolved, err := LoadAndResolveHost(t.Context(), ResolveParams{
				RepoPath: repo, Hostname: "h1", SecretsDir: t.TempDir(), DataDir: t.TempDir(),
			})
			require.Error(t, err)
			assert.Nil(t, resolved)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}
