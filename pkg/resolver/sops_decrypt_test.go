package resolver

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// openCountingFS counts the opens of one file.
type openCountingFS struct {
	fs.FS

	name  string
	opens *atomic.Int32
}

func (c openCountingFS) Open(name string) (fs.File, error) {
	if name == c.name {
		c.opens.Add(1)
	}
	return c.FS.Open(name)
}

// The Agent's decrypting reader serves one host resolve: a file reached by
// several refs (the template batch, a second template, a dynamic ref that
// only the final render pass sees and so takes the cache-miss path) is read
// and decrypted once.
func TestReadSopsSecretDecryptsEachFileOncePerResolve(t *testing.T) {
	t.Parallel()
	mapFS := pathsFleetFS("  paths: [quadlets/app.container]\n  secrets: [secrets/a.yml.tmpl, secrets/b.yml.tmpl]\n", map[string]string{ //nolint:gosec // test templates, no credentials
		"quadlets/app.container": "[Container]\nImage=docker.io/library/alpine:3\n",
		"secrets/db.sops.yaml":   sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/a.yml.tmpl":     `a: {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		// .Host.SystemdUnits is empty during the collect pass, so the
		// tokens ref is first seen by the final render (a cache miss).
		"secrets/b.yml.tmpl": `b: {{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/1" }}` +
			`{{ if .Host.SystemdUnits }} {{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0" }}{{ end }}`,
	})
	cfg, err := config.LoadAll(mapFS)
	require.NoError(t, err)

	opens := &atomic.Int32{}
	fsys := openCountingFS{FS: mapFS, name: "secrets/db.sops.yaml", opens: opens}
	identity, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sops", "identities", "srv-1.agekey"))
	require.NoError(t, err)
	ids, err := sops.ParseIdentities(identity)
	require.NoError(t, err)
	reader := sops.NewReader(fsys, ids)

	r, err := New(Config{FS: fsys, Config: cfg, Sops: reader})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	contents := map[string]string{}
	for _, f := range resolved.Files {
		contents[f.SrcPath] = f.Content
	}
	assert.Equal(t, "a: hunter2-test", contents["secrets/a.yml.tmpl"])
	assert.Equal(t, "b: beta-token alpha-token", contents["secrets/b.yml.tmpl"])
	assert.Equal(t, int32(1), opens.Load(), "the SOPS file is read and decrypted once per resolve")
	assert.Equal(t, 1, reader.DecryptedFiles())
}
