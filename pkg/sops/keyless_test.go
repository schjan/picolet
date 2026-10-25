package sops_test

import (
	"context"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// countingFS counts Open calls per name.
type countingFS struct {
	fs.FS

	opens map[string]*atomic.Int32
}

func (c countingFS) Open(name string) (fs.File, error) {
	if n, ok := c.opens[name]; ok {
		n.Add(1)
	}
	return c.FS.Open(name)
}

func keylessFS(t *testing.T) fstest.MapFS {
	t.Helper()
	tampered := strings.Replace(string(corpusFile(t, "values.sops.yaml")), "type:int]", "type:nope]", 1)
	return fstest.MapFS{
		"secrets/db.sops.yaml":       {Data: corpusFile(t, "values.sops.yaml")},
		"secrets/tampered.sops.yaml": {Data: []byte(tampered)},
		"secrets/plain.sops.yaml":    {Data: []byte("db:\n  password: hunter2\n")},
	}
}

func TestKeylessReaderPlaceholders(t *testing.T) {
	t.Parallel()
	r := sops.NewKeylessReader(keylessFS(t), hostSrv1(t))
	refs := map[string]string{
		"sops://secrets/db.sops.yaml#/db/password": sops.Placeholder,
		"sops://secrets/db.sops.yaml#/db/port":     "0",     // encrypted int
		"sops://secrets/db.sops.yaml#/db/tls":      "false", // encrypted bool
		"sops://secrets/db.sops.yaml#/db/ratio":    "0.0",   // encrypted float
		"sops://secrets/db.sops.yaml#/db/empty":    "",      // "" is never encrypted
		"sops://secrets/db.sops.yaml#/db/nothing":  "",      // null is never encrypted
		"sops://secrets/db.sops.yaml":              sops.Placeholder,
	}
	values, err := r.ReadAll(t.Context(), slices.Collect(maps.Keys(refs)))
	require.NoError(t, err)
	assert.Equal(t, refs, values)
}

func TestKeylessReaderRejectsRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ref, want string
		is              error
	}{
		{name: "not a SOPS file", ref: "sops://secrets/plain.sops.yaml#/db/password", is: sops.ErrNotSOPSFile},
		{name: "map", ref: "sops://secrets/db.sops.yaml#/db", is: sops.ErrNotLeaf},
		{name: "list", ref: "sops://secrets/db.sops.yaml#/tokens", is: sops.ErrNotLeaf},
		{name: "missing path", ref: "sops://secrets/db.sops.yaml#/db/nope", is: sops.ErrBadPointer},
		{name: "invalid type tag", ref: "sops://secrets/tampered.sops.yaml#/db/port", want: "invalid type tag"},
		{name: "missing file", ref: "sops://secrets/gone.sops.yaml#/x", want: "secrets/gone.sops.yaml"},
		{name: "no .sops. infix", ref: "sops://secrets/db.yaml#/x", is: sops.ErrNotSOPSPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values, err := sops.NewKeylessReader(keylessFS(t), hostSrv1(t)).ReadAll(t.Context(), []string{tt.ref})
			require.Error(t, err)
			assert.Empty(t, values)
			assert.Contains(t, err.Error(), tt.ref)
			assertNoPlaintext(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			if tt.want != "" {
				assert.Contains(t, err.Error(), tt.want)
			}
		})
	}
}

// ReadDirect checks direct secrets: refs: like ReadAll, but an "" or null
// leaf (unencrypted, so visible without a key) fails with ErrEmptyValue.
func TestKeylessReaderReadDirect(t *testing.T) {
	t.Parallel()
	r := sops.NewKeylessReader(keylessFS(t), hostSrv1(t))
	// A Podman secret is opaque bytes, so an encrypted leaf of any type
	// keeps the string placeholder here (no typed placeholder).
	ok := []string{
		"sops://secrets/db.sops.yaml#/db/password", "sops://secrets/db.sops.yaml",
		"sops://secrets/db.sops.yaml#/db/port", "sops://secrets/db.sops.yaml#/db/tls", "sops://secrets/db.sops.yaml#/db/ratio",
	}
	values, err := r.ReadDirect(t.Context(), ok)
	require.NoError(t, err)
	want := map[string]string{}
	for _, ref := range ok {
		want[ref] = sops.Placeholder
	}
	assert.Equal(t, want, values)

	for _, ref := range []string{"sops://secrets/db.sops.yaml#/db/empty", "sops://secrets/db.sops.yaml#/db/nothing"} {
		values, err := r.ReadDirect(t.Context(), []string{ref})
		require.ErrorIs(t, err, sops.ErrEmptyValue, ref)
		require.ErrorContains(t, err, ref)
		assert.Empty(t, values)
		assertNoPlaintext(t, err)
	}
}

// Each file is opened and inspected once per ReadAll call, however many refs
// name it; a second call reads it again.
func TestKeylessReaderReadsEachFileOncePerCall(t *testing.T) {
	t.Parallel()
	fsys := countingFS{FS: keylessFS(t), opens: map[string]*atomic.Int32{"secrets/db.sops.yaml": {}}}
	r := sops.NewKeylessReader(fsys, hostSrv1(t))
	refs := []string{
		"sops://secrets/db.sops.yaml#/db/password",
		"sops://secrets/db.sops.yaml#/db/port",
		"sops://secrets/db.sops.yaml#/db/missing",
	}
	_, err := r.ReadAll(t.Context(), refs)
	require.ErrorIs(t, err, sops.ErrBadPointer)
	assertNoPlaintext(t, err)
	assert.Equal(t, int32(1), fsys.opens["secrets/db.sops.yaml"].Load())

	_, err = r.ReadAll(t.Context(), refs[:1])
	require.NoError(t, err)
	assert.Equal(t, int32(2), fsys.opens["secrets/db.sops.yaml"].Load())
}

func TestKeylessReaderCancelledContext(t *testing.T) {
	t.Parallel()
	fsys := countingFS{FS: keylessFS(t), opens: map[string]*atomic.Int32{"secrets/db.sops.yaml": {}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	values, err := sops.NewKeylessReader(fsys, hostSrv1(t)).ReadAll(ctx, []string{ref})
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), ref)
	assert.Empty(t, values)
	assert.Equal(t, int32(0), fsys.opens["secrets/db.sops.yaml"].Load())
}

// keylessHost is a Host named name that declares the corpus recipients
// called recipients.
func keylessHost(t *testing.T, name string, recipients ...string) sops.Host {
	t.Helper()
	h := sops.Host{Name: name}
	for _, r := range recipients {
		parsed, err := sops.ParseRecipient(corpusRecipient(t, r))
		require.NoError(t, err)
		h.Recipients = append(h.Recipients, parsed)
	}
	return h
}

// hostSrv1 is the Host srv-1 with the corpus recipient srv-1, a Recipient of
// every corpus file.
func hostSrv1(t *testing.T) sops.Host {
	t.Helper()
	return keylessHost(t, "srv-1", "srv-1")
}

func recipientFS(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"secrets/db.sops.yaml":     {Data: corpusFile(t, "values.sops.yaml")},     // srv-1, srv-2, operator-1
		"secrets/groups.sops.yaml": {Data: corpusFile(t, "key-groups.sops.yaml")}, // [srv-1 operator-1] [srv-2] [srv-pq], threshold 2
		"secrets/pq.sops.yaml":     {Data: corpusFile(t, "pq.sops.yaml")},         // srv-pq, srv-1
	}
}

func TestKeylessReaderRecipientChecksPass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host sops.Host
		ref  string
	}{
		{name: "one of the file's recipients", host: keylessHost(t, "srv-2", "srv-2"), ref: "sops://secrets/db.sops.yaml#/db/password"},
		{name: "rotation: the new key is not yet a recipient", host: keylessHost(t, "srv-2", "srv-3", "srv-2"), ref: "sops://secrets/db.sops.yaml#/db/password"},
		{name: "rotation: the old key is no longer a recipient", host: keylessHost(t, "srv-2", "srv-2", "srv-3"), ref: "sops://secrets/db.sops.yaml"},
		{name: "hybrid post-quantum recipient", host: keylessHost(t, "srv-pq", "srv-pq"), ref: "sops://secrets/pq.sops.yaml#/db/password"},
		{name: "key groups: threshold reached", host: keylessHost(t, "srv-1", "srv-1", "srv-2"), ref: "sops://secrets/groups.sops.yaml#/db/password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values, err := sops.NewKeylessReader(recipientFS(t), tt.host).ReadAll(t.Context(), []string{tt.ref})
			require.NoError(t, err)
			assert.Equal(t, sops.Placeholder, values[tt.ref])
		})
	}
}

func TestKeylessReaderRecipientChecksFail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host sops.Host
		ref  string
		want []string
	}{
		{
			name: "no age_recipients",
			host: keylessHost(t, "srv-1"),
			ref:  "sops://secrets/db.sops.yaml#/db/password",
			want: []string{"Host srv-1 references sops://secrets/db.sops.yaml#/db/password but declares no age_recipients"},
		},
		{
			name: "Host is not a recipient",
			host: keylessHost(t, "srv-3", "srv-3"),
			ref:  "sops://secrets/db.sops.yaml#/db/password",
			want: []string{
				"secrets/db.sops.yaml: not a recipient: the file is not encrypted to Host srv-3",
				corpusRecipient(t, "srv-3"),
				"add one of them to the file's .sops.yaml creation rule and run `sops updatekeys secrets/db.sops.yaml`",
			},
		},
		{
			name: "key groups: too few groups",
			host: keylessHost(t, "srv-1", "srv-1"),
			ref:  "sops://secrets/groups.sops.yaml#/db/password",
			want: []string{
				"secrets/groups.sops.yaml: not a recipient: the file is encrypted to Host srv-1 in 1 of the 2 key groups it needs",
				corpusRecipient(t, "srv-1"),
				"run `sops updatekeys secrets/groups.sops.yaml`",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values, err := sops.NewKeylessReader(recipientFS(t), tt.host).ReadAll(t.Context(), []string{tt.ref})
			require.Error(t, err)
			assert.Empty(t, values)
			assert.Contains(t, err.Error(), tt.ref)
			for _, want := range tt.want {
				assert.Contains(t, err.Error(), want)
			}
			assertNoPlaintext(t, err)
		})
	}
}

func TestKeylessReaderNotRecipientIsErrNotRecipient(t *testing.T) {
	t.Parallel()
	_, err := sops.NewKeylessReader(recipientFS(t), keylessHost(t, "srv-3", "srv-3")).
		ReadAll(t.Context(), []string{"sops://secrets/db.sops.yaml#/db/password"})
	require.ErrorIs(t, err, sops.ErrNotRecipient)
	assertNoPlaintext(t, err)
}
