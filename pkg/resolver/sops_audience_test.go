package resolver

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

// A host resolve returns the SOPS files its refs reached, however they were
// reached: readSopsSecret, readSopsData, a templated ref and a direct
// secrets: entry.
func TestResolveHostRecordsSopsFiles(t *testing.T) {
	t.Parallel()
	values := sopsCorpusFile(t, "values.sops.yaml")
	tmpl := `a: {{ readSopsSecret "sops://secrets/a.sops.yaml#/db/password" }}
{{- range $k, $v := readSopsData "sops://secrets/b.sops.yaml#/db" }}
{{ $k }}: {{ $v }}
{{- end }}
c: {{ readSopsSecret (printf "sops://hosts/%s/c.sops.yaml#/db/user" .Host.Hostname) }}
`
	extra := map[string]string{
		"secrets/app.yml.tmpl":           tmpl,
		"hosts/test-host/host.yml":       sopsHostYAML(t, "srv-1"),
		"secrets/a.sops.yaml":            values,
		"secrets/b.sops.yaml":            values,
		"hosts/test-host/c.sops.yaml":    values,
		"secrets/direct.sops.yaml":       values,
		"secrets/unreferenced.sops.yaml": values,
	}
	fsys := pathsFleetFS("  secrets:\n    - secrets/app.yml.tmpl\n    - \"sops://secrets/direct.sops.yaml#/tokens/0\"\n", extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := NewKeyless(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	assert.Equal(t, []string{
		"hosts/test-host/c.sops.yaml",
		"secrets/a.sops.yaml",
		"secrets/b.sops.yaml",
		"secrets/direct.sops.yaml",
	}, resolved.SopsFiles)
}

func TestResolveHostWithoutSopsRefsRecordsNoFiles(t *testing.T) {
	t.Parallel()
	resolved, err := resolveSops(t, "plain: value\n", map[string]string{
		"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
	}, true, false)
	require.NoError(t, err)
	assert.Empty(t, resolved.SopsFiles)
}

// The record is per resolve: Hosts resolved concurrently on one Resolver
// each see only their own files (run under -race).
func TestResolveHostRecordsSopsFilesPerResolve(t *testing.T) {
	t.Parallel()
	values := sopsCorpusFile(t, "values.sops.yaml")
	extra := map[string]string{
		"secrets/app.yml.tmpl": `v: {{ readSopsSecret (printf "sops://hosts/%s/app.sops.yaml#/db/user" .Host.Hostname) }}` + "\n",
	}
	const n = 8
	for i := range n {
		host := fmt.Sprintf("host-%d", i)
		extra["hosts/"+host+"/host.yml"] = "hostname: " + host + "\nrole: node\nage_recipients:\n  - " + sopsRecipient(t, "srv-1") + "\n"
		extra["hosts/"+host+"/app.sops.yaml"] = values
	}
	fsys := pathsFleetFS("  secrets: [secrets/app.yml.tmpl]\n", extra)
	delete(fsys, "hosts/test-host/host.yml")
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := NewKeyless(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	got := make(map[string][]string, n)
	errs := make(map[string]error, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			host := fmt.Sprintf("host-%d", i)
			resolved, err := r.ResolveHost(t.Context(), host)
			mu.Lock()
			defer mu.Unlock()
			errs[host] = err
			if resolved != nil {
				got[host] = resolved.SopsFiles
			}
		})
	}
	wg.Wait()
	for host, err := range errs {
		require.NoError(t, err, host)
	}

	want := make(map[string][]string, n)
	for i := range n {
		host := fmt.Sprintf("host-%d", i)
		want[host] = []string{"hosts/" + host + "/app.sops.yaml"}
	}
	assert.Equal(t, want, got)
}
