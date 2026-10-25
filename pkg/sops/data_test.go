package sops_test

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

// ReadData returns the decrypted subtree a ref names: maps and lists keep
// their structure, and every leaf is a string in the form
// `sops decrypt --extract` prints it (null as "").
func TestReaderReadData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		want any
	}{
		{
			name: "map", ref: "sops://corpus/values.sops.yaml#/db",
			want: map[string]any{
				"user": "app", "password": "hunter2-test", "port": "5432", "tls": "true",
				"ratio": "0.75", "empty": "", "nothing": "",
			},
		},
		{
			name: "list with a nested map", ref: "sops://corpus/values.sops.yaml#/tokens",
			want: []any{"alpha-token", "beta-token", map[string]any{"name": "gamma", "value": "gamma-secret"}},
		},
		{
			name: "nested maps", ref: "sops://corpus/values.sops.yaml#/hosts",
			want: map[string]any{"srv-1.example.net": map[string]any{"password": "srv1-pass"}},
		},
		{name: "string scalar", ref: "sops://corpus/values.sops.yaml#/db/password", want: "hunter2-test"},
		{name: "int scalar", ref: "sops://corpus/values.sops.yaml#/db/port", want: "5432"},
		{name: "dotenv key", ref: "sops://corpus/values.sops.env#/DB_PASSWORD", want: "hunter2-test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := corpusReader(t, "srv-1").ReadData(t.Context(), tt.ref)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Without a fragment ReadData returns the whole document's structure.
func TestReaderReadDataWholeDocument(t *testing.T) {
	t.Parallel()
	got, err := corpusReader(t, "srv-1").ReadData(t.Context(), "sops://corpus/values.sops.yaml")
	require.NoError(t, err)
	doc, ok := got.(map[string]any)
	require.True(t, ok, "whole document is a map, got %T", got)
	assert.ElementsMatch(t, []string{"db", "tokens", "hosts", "odd_keys", "note_unencrypted"}, keysOf(doc))
	assert.Equal(t, map[string]any{"a/b": "slash-key", "m~n": "tilde-key"}, doc["odd_keys"])
	assert.Equal(t, "visible", doc["note_unencrypted"])
}

func TestReaderReadDataBadPointer(t *testing.T) {
	t.Parallel()
	_, err := corpusReader(t, "srv-1").ReadData(t.Context(), "sops://corpus/values.sops.yaml#/db/nope")
	require.ErrorIs(t, err, sops.ErrBadPointer)
	assert.Contains(t, err.Error(), "sops://corpus/values.sops.yaml#/db/nope")
	assert.Contains(t, err.Error(), `key "nope" not found at "/db"`)
	testsops.AssertNoPlaintext(t, err, morePlaintexts...)
}

func TestReaderReadDataNoIdentity(t *testing.T) {
	t.Parallel()
	_, err := sops.NewReader(os.DirFS(corpusDir), nil).ReadData(t.Context(), "sops://corpus/values.sops.yaml#/db")
	require.ErrorIs(t, err, sops.ErrNoIdentity)
	testsops.AssertNoPlaintext(t, err, morePlaintexts...)
}

// ReadAll and ReadData share the Reader's file cache: a file both read is
// decrypted once, and its leaves are scrubbed from error text.
func TestReaderReadDataSharesCache(t *testing.T) {
	t.Parallel()
	fsys := countingFS{FS: os.DirFS(corpusDir), opens: map[string]*atomic.Int32{"corpus/values.sops.yaml": {}}}
	r := sops.NewReader(fsys, testsops.Identities(t, "srv-1"))

	_, err := r.ReadData(t.Context(), "sops://corpus/values.sops.yaml#/tokens")
	require.NoError(t, err)
	_, err = r.ReadAll(t.Context(), []string{"sops://corpus/values.sops.yaml#/db/password"})
	require.NoError(t, err)
	_, err = r.ReadData(t.Context(), "sops://corpus/values.sops.yaml#/db")
	require.NoError(t, err)

	assert.Equal(t, int32(1), fsys.opens["corpus/values.sops.yaml"].Load())
	assert.Equal(t, 1, r.DecryptedFiles())
	assert.NotContains(t, r.Scrub("leaked gamma-secret"), "gamma-secret")
}

// The keyless ReadData returns the real structure (key names are cleartext)
// with every encrypted leaf replaced by the placeholder of its type (0,
// 0.0, false; the string placeholder for a string); "" and null, which SOPS
// never encrypts, render as on the Agent.
func TestKeylessReaderReadData(t *testing.T) {
	t.Parallel()
	p := sops.Placeholder
	tests := []struct {
		name string
		ref  string
		want any
	}{
		{
			name: "map", ref: "sops://secrets/db.sops.yaml#/db",
			want: map[string]any{"user": p, "password": p, "port": "0", "tls": "false", "ratio": "0.0", "empty": "", "nothing": ""},
		},
		{
			name: "list with a nested map", ref: "sops://secrets/db.sops.yaml#/tokens",
			want: []any{p, p, map[string]any{"name": p, "value": p}},
		},
		{name: "scalar", ref: "sops://secrets/db.sops.yaml#/db/password", want: p},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sops.NewKeylessReader(keylessFS(t), hostSrv1(t)).ReadData(t.Context(), tt.ref)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Every leaf is checked like a readSopsSecret leaf, and the Host's
// recipients like any ref's.
func TestKeylessReaderReadDataRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		host sops.Host
		is   error
		want string
	}{
		{name: "invalid type tag in a subtree", ref: "sops://secrets/tampered.sops.yaml#/db", want: `"/db/port" has an invalid type tag`},
		{name: "bad pointer", ref: "sops://secrets/db.sops.yaml#/db/nope", is: sops.ErrBadPointer},
		{name: "not a SOPS file", ref: "sops://secrets/plain.sops.yaml#/db", is: sops.ErrNotSOPSFile},
		{
			name: "no age_recipients", ref: "sops://secrets/db.sops.yaml#/db", host: sops.Host{Name: "web-1"},
			want: "Host web-1 references sops://secrets/db.sops.yaml#/db but declares no age_recipients",
		},
		{name: "not a recipient", ref: "sops://secrets/db.sops.yaml#/db", host: keylessHost(t, "web-1", "srv-pq"), is: sops.ErrNotRecipient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host := tt.host
			if host.Name == "" {
				host = hostSrv1(t)
			}
			_, err := sops.NewKeylessReader(keylessFS(t), host).ReadData(t.Context(), tt.ref)
			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), tt.ref)
			testsops.AssertNoPlaintext(t, err, morePlaintexts...)
		})
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
