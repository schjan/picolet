package resolver

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

// resolveSops resolves test-host of a one-host Fleet whose Host gets the
// secret template secrets/app.yml.tmpl with body tmpl; files adds Fleet-root
// files (corpus copies). keyless passes the keyless SOPS reader, as validate
// and resolve do; without it no SOPS reader is passed, as on the Agent.
func resolveSops(t *testing.T, tmpl string, files map[string]string, keyless, strict bool) (*ResolvedHost, error) {
	t.Helper()
	extra := map[string]string{"secrets/app.yml.tmpl": tmpl}
	maps.Copy(extra, files)
	fsys := pathsFleetFS("  secrets: [secrets/app.yml.tmpl]\n", extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc := Config{FS: fsys, Config: cfg, Strict: strict}
	if keyless {
		rc.SopsSecretReader = KeylessSopsReader(fsys)
	}
	r, err := New(rc)
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
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

// Templated refs and whole-file refs work keyless; "" and null leaves, which
// SOPS never encrypts, pass for template refs.
func TestReadSopsSecretKeylessAcceptedRefs(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, call string }{
		{name: "templated path", call: `readSopsSecret (print "sops://hosts/" .Host.Hostname "/app.sops.yaml#/db/password")`},
		{name: "list index", call: `readSopsSecret "sops://secrets/db.sops.yaml#/tokens/0"`},
		{name: "escaped key", call: `readSopsSecret "sops://secrets/db.sops.yaml#/odd_keys/a~1b"`},
		{name: "int leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/port"`},
		{name: "empty leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/empty"`},
		{name: "null leaf", call: `readSopsSecret "sops://secrets/db.sops.yaml#/db/nothing"`},
		{name: "whole file", call: `readSopsSecret "sops://secrets/tls.sops.pem"`},
		{name: "dotenv key", call: `readSopsSecret "sops://secrets/app.sops.env#/DB_PASSWORD"`},
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
			assert.Equal(t, "v: <sops-secret>", resolved.Files[0].Content)
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
		{name: "no sops infix", ref: "sops://secrets/db.yaml#/db/password", want: "needs `.sops.` before its extension"},
		{name: "dot-dot", ref: "sops://secrets/../secrets/db.sops.yaml#/db/password", want: "clean relative path"},
		{name: "absolute", ref: "sops:///etc/db.sops.yaml#/db/password", want: "clean relative path"},
		{name: "plaintext sops file", ref: "sops://secrets/plain.sops.yaml#/db/password", want: "not a SOPS file"},
		{name: "missing key", ref: "sops://secrets/db.sops.yaml#/db/nope", want: `key "nope" not found at "/db"`},
		{name: "map", ref: "sops://secrets/db.sops.yaml#/db", want: `"/db" is a map; a string ref must end at a leaf`},
		{name: "list", ref: "sops://secrets/db.sops.yaml#/tokens", want: `"/tokens" is a list; a string ref must end at a leaf`},
		{name: "unencrypted leaf", ref: "sops://secrets/db.sops.yaml#/note_unencrypted", want: `"/note_unencrypted" is not encrypted`},
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
