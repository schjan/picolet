package resolver

import (
	"io/fs"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	r, probe := sopsResolver(t, "  paths: [quadlets/app.container]\n  secrets: [secrets/a.yml.tmpl, secrets/b.yml.tmpl]\n", map[string]string{ //nolint:gosec // test templates, no credentials
		"quadlets/app.container": "[Container]\nImage=docker.io/library/alpine:3\n",
		"secrets/a.yml.tmpl":     `a: {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		// .Host.SystemdUnits is empty during the collect pass, so the
		// tokens ref is first seen by the final render (a cache miss).
		"secrets/b.yml.tmpl": `b: {{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/1" }}` +
			`{{ if .Host.SystemdUnits }} {{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0" }}{{ end }}`,
	}, sopsDecrypting, false)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	contents := map[string]string{}
	for _, f := range resolved.Files {
		contents[f.SrcPath] = f.Content
	}
	assert.Equal(t, "a: hunter2-test", contents["secrets/a.yml.tmpl"])
	assert.Equal(t, "b: beta-token alpha-token", contents["secrets/b.yml.tmpl"])
	assert.Equal(t, int32(1), probe.opens.Load(), "the SOPS file is read and decrypted once per resolve")
	assert.Equal(t, 1, probe.reader.DecryptedFiles())
}

// The keyless reader serves one host resolve too: a file reached by
// readSopsSecret, readSopsData and a direct secrets: entry is read and
// inspected once.
func TestKeylessReadsEachSopsFileOncePerResolve(t *testing.T) {
	t.Parallel()
	r, probe := sopsResolver(t, "  paths: [quadlets/app.container]\n"+
		"  secrets: [secrets/a.yml.tmpl, \"sops://secrets/db.sops.yaml#/db/user\"]\n", map[string]string{
		"quadlets/app.container": "[Container]\nImage=docker.io/library/alpine:3\n",
		"secrets/a.yml.tmpl": `a: {{ readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0" }}` +
			`{{ range $k, $v := readSopsData "sops://secrets/db.sops.yaml#/hosts" }} {{ $k }}{{ end }}`,
	}, sopsKeyless, false)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	contents := map[string]string{}
	for _, f := range resolved.Files {
		contents[f.DestPath] = f.Content
	}
	assert.Equal(t, "a: <sops-secret> srv-1.example.net", contents["secret:a"])
	assert.Equal(t, sops.Placeholder, contents["secret:db_user"])
	assert.Equal(t, int32(1), probe.opens.Load(), "the SOPS file is read and inspected once per resolve")
}
