package resolver

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// resolveSops resolves test-host of a one-host Fleet whose Host gets the
// secret template secrets/app.yml.tmpl with body tmpl and declares the corpus
// recipient srv-1 (a Recipient of every corpus file); files adds Fleet-root
// files (corpus copies) and may replace hosts/test-host/host.yml. keyless
// resolves with NewKeyless, as validate and resolve do; without it no SOPS
// reader is passed, as on the Agent.
func resolveSops(t *testing.T, tmpl string, files map[string]string, keyless, strict bool) (*ResolvedHost, error) {
	t.Helper()
	extra := map[string]string{
		"secrets/app.yml.tmpl":     tmpl,
		"hosts/test-host/host.yml": sopsHostYAML(t, "srv-1"),
	}
	maps.Copy(extra, files)
	fsys := pathsFleetFS("  secrets: [secrets/app.yml.tmpl]\n", extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc := Config{FS: fsys, Config: cfg, Strict: strict}
	newResolver := New
	if keyless {
		newResolver = NewKeyless
	}
	r, err := newResolver(rc)
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}

// sopsRecipient returns the TEST-ONLY corpus recipient called name.
func sopsRecipient(t *testing.T, name string) string {
	t.Helper()
	for line := range strings.Lines(sopsCorpusText(t, "recipients.txt")) {
		if n, key, ok := strings.Cut(strings.TrimSpace(line), " "); ok && n == name {
			return key
		}
	}
	t.Fatalf("recipient %q not in corpus", name)
	return ""
}

// sopsHostYAML is test-host's host.yml declaring the corpus recipients
// called recipients (none: no age_recipients: key).
func sopsHostYAML(t *testing.T, recipients ...string) string {
	t.Helper()
	y := "hostname: test-host\nexternal_hostname: test-host.example.net\nrole: node\nfeatures: []\n"
	if len(recipients) > 0 {
		y += "age_recipients:\n"
		for _, r := range recipients {
			y += "  - " + sopsRecipient(t, r) + "\n"
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
		"secrets/db.sops.yaml":     sopsCorpusFile(t, "values.sops.yaml"),     // srv-1, srv-2, operator-1
		"secrets/groups.sops.yaml": sopsCorpusFile(t, "key-groups.sops.yaml"), // [srv-1 operator-1] [srv-2] [srv-pq], threshold 2
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
				sopsRecipient(t, "srv-3"),
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
			resolved, err := resolveSops(t, `v: {{ readSopsSecret "`+tt.ref+`" }}`, fleet, true, false)
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
			for _, plain := range []string{"hunter2-test", "visible", "5432"} {
				assert.NotContains(t, err.Error(), plain)
			}
		})
	}
}

// Template data exposes the Host's age recipients in canonical form, for
// .Host and for every .Fleet.Hosts entry.
func TestTemplateDataAgeRecipients(t *testing.T) {
	t.Parallel()
	hostYAML := strings.Replace(sopsHostYAML(t, "srv-1", "srv-pq"), sopsRecipient(t, "srv-1"), strings.ToUpper(sopsRecipient(t, "srv-1")), 1)
	resolved, err := resolveSops(t,
		`{{ join "," .Host.AgeRecipients }}|{{ range .Fleet.Hosts }}{{ len .AgeRecipients }}{{ end }}`,
		map[string]string{"hosts/test-host/host.yml": hostYAML}, true, false)
	require.NoError(t, err)
	assert.Equal(t, sopsRecipient(t, "srv-1")+","+sopsRecipient(t, "srv-pq")+"|2", resolved.Files[0].Content)
}

func TestReadSopsSecretKeylessRendersPlaceholder(t *testing.T) {
	t.Parallel()
	resolved, err := resolveSops(t,
		`password: {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`,
		map[string]string{"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml")}, true, false)
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, "password: <sops-secret>", resolved.Files[0].Content)
}

// Templated refs and whole-file refs work keyless. Leaves the file leaves
// in plaintext ("" and null, which SOPS never encrypts, and those its
// encryption rules exclude) render their value, as on the Agent.
func TestReadSopsSecretKeylessAcceptedRefs(t *testing.T) {
	t.Parallel()
	p := sops.Placeholder
	tests := []struct{ name, call, want string }{
		{name: "templated path", call: `readSopsSecret (print "sops://hosts/" .Host.Hostname "/app.sops.yaml#/db/password")`, want: p},
		{name: "list index", call: `readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0"`, want: p},
		{name: "escaped key", call: `readSopsSecret "sops://secrets/db.sops.yaml#/odd_keys/a~1b"`, want: p},
		{name: "int leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/port"`, want: p},
		{name: "empty leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/empty"`},
		{name: "null leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/nothing"`},
		{name: "unencrypted leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/note_unencrypted"`, want: "visible"},
		{name: "whole file", call: `readSopsSecret "sops://secrets/tls.sops.pem"`, want: p},
		{name: "dotenv key", call: `readSopsSecret "sops://secrets/app.sops.env#/DB_PASSWORD"`, want: p},
	}
	files := map[string]string{
		"secrets/db.sops.yaml":          sopsCorpusFile(t, "values.sops.yaml"),
		"hosts/test-host/app.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/tls.sops.pem":          sopsCorpusFile(t, "tls.sops.pem"),
		"secrets/app.sops.env":          sopsCorpusFile(t, "values.sops.env"),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveSops(t, "v: {{ "+tt.call+" }}", files, true, false)
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
	tampered := strings.Replace(sopsCorpusFile(t, "values.sops.yaml"), "type:int]", "type:nope]", 1)
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
		"secrets/db.sops.yaml":       sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/db.yaml":            sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/plain.sops.yaml":    sopsCorpusFile(t, "plaintext.sops.yaml"),
		"secrets/tampered.sops.yaml": tampered,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveSops(t, `v: {{ readSopsSecret "`+tt.ref+`" }}`, files, true, false)
			require.Error(t, err)
			require.ErrorContains(t, err, tt.ref)
			require.ErrorContains(t, err, tt.want)
			for _, plain := range []string{"hunter2-test", "visible", "5432"} {
				assert.NotContains(t, err.Error(), plain)
			}
		})
	}
}

// Without a SOPS reader (every Agent path until decryption ships, and an
// Agent without a sops: block) a sops:// ref fails the resolve: the Agent
// never deploys a placeholder as a value.
func TestReadSopsSecretWithoutReaderFails(t *testing.T) {
	t.Parallel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	resolved, err := resolveSops(t, `password: {{ readSopsSecret "`+ref+`" }}`,
		map[string]string{"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml")}, false, false)
	require.Error(t, err)
	assert.Nil(t, resolved)
	require.ErrorContains(t, err, ref)
	require.ErrorContains(t, err, "no age identity configured; set sops.age_identity_path")
	assert.NotContains(t, err.Error(), "<sops-secret")
}

// Strict mode (bootstrap, validate's Agent-bundle pass) fails on
// readSopsSecret with the SOPS fix, even with the keyless reader passed.
func TestReadSopsSecretStrictFails(t *testing.T) {
	t.Parallel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	for _, keyless := range []bool{true, false} {
		t.Run(fmt.Sprintf("keyless=%v", keyless), func(t *testing.T) {
			t.Parallel()
			_, err := resolveSops(t, `password: {{ readSopsSecret "`+ref+`" }}`,
				map[string]string{"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml")}, keyless, true)
			require.Error(t, err)
			require.ErrorContains(t, err, fmt.Sprintf("readSopsSecret %q", ref))
			require.ErrorContains(t, err, "deliver the value as a --secrets-dir file and point the Agent config at its path; "+
				"credentials needed to fetch the Fleet can't be SOPS values")
		})
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
	require.ErrorContains(t, err, `readOpSecret "op://vault/item/pw": provider not configured in strict mode`)
	assert.False(t, called)
}
