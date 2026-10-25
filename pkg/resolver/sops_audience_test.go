package resolver

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
)

// A host resolve returns the SOPS files its refs reached, however they were
// reached: readSopsSecret, readSopsData, a templated ref and a direct
// secrets: entry.
func TestResolveHostRecordsSopsFiles(t *testing.T) {
	t.Parallel()
	values := testsops.CorpusText(t, "values.sops.yaml")
	tmpl := `a: {{ readSopsSecret "sops://secrets/a.sops.yaml#/db/password" }}
{{- range $k, $v := readSopsData "sops://secrets/b.sops.yaml#/db" }}
{{ $k }}: {{ $v }}
{{- end }}
c: {{ readSopsSecret (printf "sops://hosts/%s/c.sops.yaml#/db/user" .Host.Hostname) }}
`
	resolved, err := resolveSopsFleet(t, secretsBase(appTmpl, "sops://secrets/direct.sops.yaml#/tokens/0"), map[string]string{
		appTmpl:                          tmpl,
		"secrets/a.sops.yaml":            values,
		"secrets/b.sops.yaml":            values,
		"hosts/test-host/c.sops.yaml":    values,
		"secrets/direct.sops.yaml":       values,
		"secrets/unreferenced.sops.yaml": values,
	}, sopsKeyless, false)
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
	resolved, err := resolveSopsFleet(t, appTmplBase, map[string]string{appTmpl: "plain: value\n"}, sopsKeyless, false)
	require.NoError(t, err)
	assert.Empty(t, resolved.SopsFiles)
}

// The record is per resolve: Hosts resolved concurrently on one Resolver
// each see only their own files (run under -race).
func TestResolveHostRecordsSopsFilesPerResolve(t *testing.T) {
	t.Parallel()
	values := testsops.CorpusText(t, "values.sops.yaml")
	extra := map[string]string{
		appTmpl: `v: {{ readSopsSecret (printf "sops://hosts/%s/app.sops.yaml#/db/user" .Host.Hostname) }}` + "\n",
	}
	const n = 8
	for i := range n {
		host := fmt.Sprintf("host-%d", i)
		extra["hosts/"+host+"/host.yml"] = "hostname: " + host + "\nrole: node\nage_recipients:\n  - " + testsops.Recipient(t, "srv-1") + "\n"
		extra["hosts/"+host+"/app.sops.yaml"] = values
	}
	r, _ := sopsResolver(t, appTmplBase, extra, sopsKeyless, false)

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
