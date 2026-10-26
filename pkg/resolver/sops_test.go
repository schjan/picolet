package resolver

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// sopsReaderMode says which SOPS reader a test resolve passes.
type sopsReaderMode int

const (
	sopsNoReader   sopsReaderMode = iota // the Agent without a sops: block
	sopsKeyless                          // validate, resolve (NewKeyless)
	sopsDecrypting                       // the Agent with the TEST-ONLY identity srv-1
	sopsRecording                        // the Agent, through a recordingSopsReader
)

// String names the mode in subtest names.
func (m sopsReaderMode) String() string {
	switch m {
	case sopsNoReader:
		return "no-reader"
	case sopsKeyless:
		return "keyless"
	case sopsDecrypting:
		return "decrypting"
	case sopsRecording:
		return "recording"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// appTmpl is the secret template most SOPS tests render, and appTmplBase
// the base assignment that delivers it.
const (
	appTmpl     = "secrets/app.yml.tmpl"
	appTmplBase = "  secrets: [" + appTmpl + "]\n"
)

// secretsBase is a base assignment listing secrets (file secrets and
// direct sops:// refs).
func secretsBase(secrets ...string) string {
	base := "  secrets:\n"
	for _, s := range secrets {
		base += "    - \"" + s + "\"\n"
	}
	return base
}

// sopsProbe is what a sopsResolver resolve can be observed through.
type sopsProbe struct {
	opens  *atomic.Int32        // opens of secrets/db.sops.yaml, by the resolver and its reader
	reader *sops.Reader         // the decrypting reader (sopsDecrypting only)
	rec    *recordingSopsReader // asked only in sopsRecording mode
}

// sopsResolver builds a Resolver over a one-host Fleet with assignment base
// body base whose Host test-host declares the corpus recipient srv-1 (a
// Recipient of every corpus file), and that holds the corpus file
// values.sops.yaml at secrets/db.sops.yaml; files adds Fleet-root files and
// may replace either. mode picks the SOPS reader.
func sopsResolver(t *testing.T, base string, files map[string]string, mode sopsReaderMode, strict bool) (*Resolver, sopsProbe) {
	t.Helper()
	extra := map[string]string{
		"secrets/db.sops.yaml":     testsops.CorpusText(t, "values.sops.yaml"),
		"hosts/test-host/host.yml": sopsHostYAML(t, "srv-1"),
	}
	maps.Copy(extra, files)
	mapFS := pathsFleetFS(base, extra)
	cfg, err := config.LoadAll(mapFS)
	require.NoError(t, err)
	probe := sopsProbe{opens: &atomic.Int32{}, rec: &recordingSopsReader{}}
	fsys := openCountingFS{FS: mapFS, name: "secrets/db.sops.yaml", opens: probe.opens}
	rc := Config{FS: fsys, Config: cfg, Strict: strict}
	newResolver := New
	switch mode {
	case sopsNoReader:
	case sopsKeyless:
		newResolver = NewKeyless
	case sopsDecrypting:
		probe.reader = sops.NewReader(fsys, testsops.Identities(t, "srv-1"))
		rc.Sops = probe.reader
	case sopsRecording:
		rc.Sops = probe.rec
	}
	r, err := newResolver(rc)
	require.NoError(t, err)
	return r, probe
}

// resolveSopsFleet resolves test-host of sopsResolver's Fleet.
func resolveSopsFleet(t *testing.T, base string, files map[string]string, mode sopsReaderMode, strict bool) (*ResolvedHost, error) {
	t.Helper()
	r, _ := sopsResolver(t, base, files, mode, strict)
	return r.ResolveHost(t.Context(), "test-host")
}

// sopsHostYAML is test-host's host.yml declaring the corpus recipients
// called recipients (none: no age_recipients: key).
func sopsHostYAML(t *testing.T, recipients ...string) string {
	t.Helper()
	y := "hostname: test-host\nexternal_hostname: test-host.example.net\nrole: node\nfeatures: []\n"
	if len(recipients) > 0 {
		y += "age_recipients:\n"
		for _, r := range recipients {
			y += "  - " + testsops.Recipient(t, r) + "\n"
		}
	}
	return y
}

// The keyless per-ref recipient checks: the Host must declare
// age_recipients:, and one of them must reach enough of the file's key
// groups; during a rotation either key passes.
func TestReadSopsSecretKeylessRecipientChecks(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		// secrets/db.sops.yaml (sopsResolver's): srv-1, srv-2, operator-1
		"secrets/groups.sops.yaml": testsops.CorpusText(t, "key-groups.sops.yaml"), // [srv-1 operator-1] [srv-2] [srv-pq], threshold 2
	}
	tests := []struct {
		name       string
		recipients []string
		ref        string
		want       []string // empty: the resolve passes
	}{
		{name: "declared recipient", recipients: []string{"srv-2"}, ref: "sops://secrets/db.sops.yaml#/db/password"},
		{name: "rotation", recipients: []string{"srv-3", "srv-1"}, ref: "sops://secrets/db.sops.yaml#/db/password"},
		{name: "key groups: threshold reached", recipients: []string{"srv-1", "srv-pq"}, ref: "sops://secrets/groups.sops.yaml#/db/password"},
		{
			name: "no age_recipients", ref: "sops://secrets/db.sops.yaml#/db/password",
			want: []string{"Host test-host references sops://secrets/db.sops.yaml#/db/password but declares no age_recipients"},
		},
		{
			name: "drift", recipients: []string{"srv-3"}, ref: "sops://secrets/db.sops.yaml#/db/password",
			want: []string{
				"secrets/db.sops.yaml: not a recipient: the file is not encrypted to Host test-host",
				testsops.Recipient(t, "srv-3"),
				"add one of them to the file's .sops.yaml creation rule and run `sops updatekeys secrets/db.sops.yaml`",
			},
		},
		{
			name: "key groups: too few", recipients: []string{"srv-2"}, ref: "sops://secrets/groups.sops.yaml#/db/password",
			want: []string{"the file is encrypted to Host test-host in 1 of the 2 key groups it needs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fleet := maps.Clone(files)
			fleet["hosts/test-host/host.yml"] = sopsHostYAML(t, tt.recipients...)
			fleet[appTmpl] = `v: {{ readSopsSecret "` + tt.ref + `" }}`
			resolved, err := resolveSopsFleet(t, appTmplBase, fleet, sopsKeyless, false)
			if len(tt.want) == 0 {
				require.NoError(t, err)
				assert.Equal(t, "v: <sops-secret>", resolved.Files[0].Content)
				return
			}
			require.Error(t, err)
			require.ErrorContains(t, err, tt.ref)
			for _, want := range tt.want {
				require.ErrorContains(t, err, want)
			}
			testsops.AssertNoPlaintext(t, err)
		})
	}
}

// Template data exposes the Host's age recipients in canonical form, for
// .Host and for every .Fleet.Hosts entry.
func TestTemplateDataAgeRecipients(t *testing.T) {
	t.Parallel()
	hostYAML := strings.Replace(sopsHostYAML(t, "srv-1", "srv-pq"), testsops.Recipient(t, "srv-1"), strings.ToUpper(testsops.Recipient(t, "srv-1")), 1)
	resolved, err := resolveSopsFleet(t, appTmplBase, map[string]string{
		appTmpl:                    `{{ join "," .Host.AgeRecipients }}|{{ range .Fleet.Hosts }}{{ len .AgeRecipients }}{{ end }}`,
		"hosts/test-host/host.yml": hostYAML,
	}, sopsKeyless, false)
	require.NoError(t, err)
	assert.Equal(t, testsops.Recipient(t, "srv-1")+","+testsops.Recipient(t, "srv-pq")+"|2", resolved.Files[0].Content)
}

func TestReadSopsSecretKeylessRendersPlaceholder(t *testing.T) {
	t.Parallel()
	resolved, err := resolveSopsFleet(t, appTmplBase, map[string]string{
		appTmpl: `password: {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
	}, sopsKeyless, false)
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, "password: <sops-secret>", resolved.Files[0].Content)
}

// Templated refs and whole-file refs work keyless. Leaves the file leaves
// in plaintext ("" and null, which SOPS never encrypts, and those its
// encryption rules exclude) render their value, as on the Agent. An
// encrypted int, bool or float leaf renders as a value of its type.
func TestReadSopsSecretKeylessAcceptedRefs(t *testing.T) {
	t.Parallel()
	p := sops.Placeholder
	tests := []struct{ name, call, want string }{
		{name: "templated path", call: `readSopsSecret (print "sops://hosts/" .Host.Hostname "/app.sops.yaml#/db/password")`, want: p},
		{name: "list index", call: `readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0"`, want: p},
		{name: "escaped key", call: `readSopsSecret "sops://secrets/db.sops.yaml#/odd_keys/a~1b"`, want: p},
		{name: "int leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/port"`, want: "0"},
		{name: "bool leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/tls"`, want: "false"},
		{name: "float leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/ratio"`, want: "0.0"},
		{name: "empty leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/empty"`},
		{name: "null leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/nothing"`},
		{name: "unencrypted leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/note_unencrypted"`, want: "visible"},
		{name: "whole file", call: `readSopsSecret "sops://secrets/tls.sops.pem"`, want: p},
		{name: "dotenv key", call: `readSopsSecret "sops://secrets/app.sops.env#/DB_PASSWORD"`, want: p},
	}
	files := map[string]string{
		"hosts/test-host/app.sops.yaml": testsops.CorpusText(t, "values.sops.yaml"),
		"secrets/tls.sops.pem":          testsops.CorpusText(t, "tls.sops.pem"),
		"secrets/app.sops.env":          testsops.CorpusText(t, "values.sops.env"),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fleet := maps.Clone(files)
			fleet[appTmpl] = "v: {{ " + tt.call + " }}"
			resolved, err := resolveSopsFleet(t, appTmplBase, fleet, sopsKeyless, false)
			require.NoError(t, err)
			require.Len(t, resolved.Files, 1)
			assert.Equal(t, "v: "+tt.want, resolved.Files[0].Content)
		})
	}
}

// The keyless per-ref checks fail the resolve with an error that names the
// ref, and never quote a value of the file.
func TestReadSopsSecretKeylessRejectedRefs(t *testing.T) {
	t.Parallel()
	tampered := strings.Replace(testsops.CorpusText(t, "values.sops.yaml"), "type:int]", "type:nope]", 1)
	tests := []struct{ name, ref, want string }{
		{name: "missing file", ref: "sops://secrets/gone.sops.yaml#/db/password", want: "file does not exist"},
		{name: "no sops infix", ref: "sops://secrets/db.yaml#/db/password", want: "needs `.sops.` directly before its final extension"},
		{name: "dot-dot", ref: "sops://secrets/../secrets/db.sops.yaml#/db/password", want: "clean relative path"},
		{name: "absolute", ref: "sops:///etc/db.sops.yaml#/db/password", want: "clean relative path"},
		{name: "plaintext sops file", ref: "sops://secrets/plain.sops.yaml#/db/password", want: "not a SOPS file"},
		{name: "missing key", ref: "sops://secrets/db.sops.yaml#/db/nope", want: `key "nope" not found at "/db"`},
		{name: "map", ref: "sops://secrets/db.sops.yaml#/db", want: `"/db" is a map; a string ref must end at a leaf`},
		{name: "list", ref: "sops://secrets/db.sops.yaml#/tokens", want: `"/tokens" is a list; a string ref must end at a leaf`},
		{name: "invalid type tag", ref: "sops://secrets/tampered.sops.yaml#/db/port", want: `"/db/port" has an invalid type tag`},
	}
	files := map[string]string{
		"secrets/db.yaml":            testsops.CorpusText(t, "values.sops.yaml"),
		"secrets/plain.sops.yaml":    testsops.CorpusText(t, "plaintext.sops.yaml"),
		"secrets/tampered.sops.yaml": tampered,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fleet := maps.Clone(files)
			fleet[appTmpl] = `v: {{ readSopsSecret "` + tt.ref + `" }}`
			_, err := resolveSopsFleet(t, appTmplBase, fleet, sopsKeyless, false)
			require.Error(t, err)
			require.ErrorContains(t, err, tt.ref)
			require.ErrorContains(t, err, tt.want)
			testsops.AssertNoPlaintext(t, err)
		})
	}
}

// Without a SOPS reader (an Agent without a sops: block) a sops:// ref
// fails the resolve with the no-identity error: the Agent never deploys a
// placeholder as a value, and a direct ref is never skipped like an
// unconfigured op:// ref. Strict mode (bootstrap, validate's Agent-bundle
// pass) fails it with the SOPS fix, even with the keyless reader passed;
// Service Bundles cannot carry direct secrets: entries, so this seam is where
// the Agent-bundle pass's direct-ref branch is covered. Each error names the
// call and its ref's file (a template may build the pointer from a value),
// or the direct entry and its ref, and never quotes a value.
func TestSopsRefFailsWithoutReaderAndInStrictMode(t *testing.T) {
	t.Parallel()
	const (
		leaf     = "sops://secrets/db.sops.yaml#/db/password"
		tree     = "sops://secrets/db.sops.yaml#/db"
		noIdent  = ": no age identity configured; set sops.age_identity_path in the Agent config"
		strictly = ": SOPS values can't be used in strict mode; " +
			"deliver the value as a --secrets-dir file and point the Agent config at its path; " +
			"credentials needed to fetch the Fleet can't be SOPS values"
	)
	kinds := []struct {
		name, base string
		files      map[string]string
		call       string
	}{
		{
			name: "secret", base: appTmplBase, call: `readSopsSecret "sops://secrets/db.sops.yaml#…"`,
			files: map[string]string{appTmpl: `password: {{ readSopsSecret "` + leaf + `" }}`},
		},
		{
			name: "data", base: appTmplBase, call: `readSopsData "sops://secrets/db.sops.yaml#…"`,
			files: map[string]string{appTmpl: `{{ range readSopsData "` + tree + `" }}{{ . }}{{ end }}`},
		},
		{name: "direct", base: secretsBase(leaf), call: "direct secrets " + leaf},
	}
	runs := []struct {
		name   string
		mode   sopsReaderMode
		strict bool
		want   string
	}{
		{name: "no-reader", mode: sopsNoReader, want: noIdent},
		{name: "strict/keyless", mode: sopsKeyless, strict: true, want: strictly},
		{name: "strict/no-reader", mode: sopsNoReader, strict: true, want: strictly},
	}
	for _, k := range kinds {
		for _, run := range runs {
			t.Run(k.name+"/"+run.name, func(t *testing.T) {
				t.Parallel()
				resolved, err := resolveSopsFleet(t, k.base, k.files, run.mode, run.strict)
				require.Error(t, err)
				assert.Nil(t, resolved)
				require.ErrorContains(t, err, k.call+run.want)
				if !run.strict {
					require.ErrorIs(t, err, sops.ErrNoIdentity)
				}
				assert.NotContains(t, err.Error(), "<sops-secret")
				testsops.AssertNoPlaintext(t, err)
			})
		}
	}
}

// Strict mode fails a provider call before looking at the provider's reader:
// op:// with a configured reader fails too, without calling it (bootstrap and
// validate's Agent-bundle pass render what the Agent gets before it has any
// provider).
func TestStrictModeFailsEvenWithReader(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"s.tmpl": &fstest.MapFile{Data: []byte(`pw={{readOpSecret "op://vault/item/pw"}}`)}}
	called := false
	reader := func(context.Context, []string) (map[string]string, error) {
		called = true
		return map[string]string{"op://vault/item/pw": "real"}, nil
	}
	p := OpProvider(reader)
	p.Strict = true
	tmpl, _, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{p}, "")
	require.NoError(t, err)
	err = tmpl.ExecuteTemplate(io.Discard, "s.tmpl", nil)
	require.ErrorContains(t, err, `readOpSecret "op://vault/…": provider not configured in strict mode`)
	assert.False(t, called)
}
