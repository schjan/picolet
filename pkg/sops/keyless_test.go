package sops_test

import (
	"context"
	"io/fs"
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
	r := sops.NewKeylessReader(keylessFS(t))
	refs := []string{
		"sops://secrets/db.sops.yaml#/db/password",
		"sops://secrets/db.sops.yaml#/db/port",
		"sops://secrets/db.sops.yaml#/db/empty",   // "" is never encrypted
		"sops://secrets/db.sops.yaml#/db/nothing", // null is never encrypted
		"sops://secrets/db.sops.yaml",
	}
	values, err := r.ReadAll(t.Context(), refs)
	require.NoError(t, err)
	for _, ref := range refs {
		assert.Equal(t, sops.Placeholder, values[ref], ref)
	}
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
		{name: "unencrypted leaf", ref: "sops://secrets/db.sops.yaml#/note_unencrypted", want: "is not encrypted"},
		{name: "invalid type tag", ref: "sops://secrets/tampered.sops.yaml#/db/port", want: "invalid type tag"},
		{name: "missing file", ref: "sops://secrets/gone.sops.yaml#/x", want: "secrets/gone.sops.yaml"},
		{name: "no .sops. infix", ref: "sops://secrets/db.yaml#/x", is: sops.ErrNotSOPSPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values, err := sops.NewKeylessReader(keylessFS(t)).ReadAll(t.Context(), []string{tt.ref})
			require.Error(t, err)
			assert.Empty(t, values)
			assert.Contains(t, err.Error(), tt.ref)
			assert.NotContains(t, err.Error(), "hunter2")
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			if tt.want != "" {
				assert.Contains(t, err.Error(), tt.want)
			}
		})
	}
}

// Each file is opened and inspected once per ReadAll call, however many refs
// name it; a second call reads it again.
func TestKeylessReaderReadsEachFileOncePerCall(t *testing.T) {
	t.Parallel()
	fsys := countingFS{FS: keylessFS(t), opens: map[string]*atomic.Int32{"secrets/db.sops.yaml": {}}}
	r := sops.NewKeylessReader(fsys)
	refs := []string{
		"sops://secrets/db.sops.yaml#/db/password",
		"sops://secrets/db.sops.yaml#/db/port",
		"sops://secrets/db.sops.yaml#/db/missing",
	}
	_, err := r.ReadAll(t.Context(), refs)
	require.ErrorIs(t, err, sops.ErrBadPointer)
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
	values, err := sops.NewKeylessReader(fsys).ReadAll(ctx, []string{ref})
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), ref)
	assert.Empty(t, values)
	assert.Equal(t, int32(0), fsys.opens["secrets/db.sops.yaml"].Load())
}
