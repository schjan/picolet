package sops_test

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// A Reader serves one resolve: each file is read and decrypted at most once
// however many ReadAll calls reach it (the template batch, cache misses),
// and a failed file is not retried either.
func TestReaderDecryptsEachFileOncePerReader(t *testing.T) {
	t.Parallel()
	fsys := countingFS{FS: os.DirFS(corpusDir), opens: map[string]*atomic.Int32{
		"corpus/values.sops.yaml": {}, "corpus/tampered-mac.sops.yaml": {},
	}}
	r := sops.NewReader(fsys, corpusIdentities(t, "srv-1"))

	_, err := r.ReadAll(t.Context(), []string{
		"sops://corpus/values.sops.yaml#/db/password",
		"sops://corpus/values.sops.yaml#/tokens/0",
	})
	require.NoError(t, err)
	got, err := r.ReadAll(t.Context(), []string{"sops://corpus/values.sops.yaml#/tokens/1"})
	require.NoError(t, err)
	assert.Equal(t, "beta-token", got["sops://corpus/values.sops.yaml#/tokens/1"])

	for range 2 {
		_, err = r.ReadAll(t.Context(), []string{"sops://corpus/tampered-mac.sops.yaml#/db/password"})
		require.ErrorIs(t, err, sops.ErrEditedOutsideSOPS)
	}

	assert.Equal(t, int32(1), fsys.opens["corpus/values.sops.yaml"].Load())
	assert.Equal(t, int32(1), fsys.opens["corpus/tampered-mac.sops.yaml"].Load())
}

// DecryptedFiles counts the files this Reader decrypted successfully, the
// Agent's definition of a SOPS sync.
func TestReaderDecryptedFiles(t *testing.T) {
	t.Parallel()
	var none *sops.Reader
	assert.Zero(t, none.DecryptedFiles())

	r := corpusReader(t, "srv-1")
	assert.Zero(t, r.DecryptedFiles())
	_, err := r.ReadAll(t.Context(), []string{
		"sops://corpus/values.sops.yaml#/db/password",
		"sops://corpus/values.sops.yaml#/db/nope",
		"sops://corpus/tls.sops.pem",
		"sops://corpus/tampered-mac.sops.yaml#/db/password",
	})
	require.Error(t, err)
	assert.Equal(t, 2, r.DecryptedFiles())
}

// Scrub removes every decrypted leaf value of four characters or more, of
// every file the Reader decrypted (not only the leaves a ref named), in the
// spellings errors embed them: as is, Go-quoted, line by line, and as the
// YAML library's truncated "first seven..." form.
func TestReaderScrub(t *testing.T) {
	t.Parallel()
	r := corpusReader(t, "srv-1")
	_, err := r.ReadAll(t.Context(), []string{
		"sops://corpus/values.sops.yaml#/db/password",
		"sops://corpus/tls.sops.pem",
	})
	require.NoError(t, err)
	pem, err := os.ReadFile(filepath.Join(corpusDir, "plain", "tls.pem"))
	require.NoError(t, err)

	tests := []struct{ in, want string }{
		{in: "kind \"hunter2-test\" not supported", want: "kind \"<redacted>\" not supported"},
		{in: "token alpha-token and gamma-secret", want: "token <redacted> and <redacted>"},
		{in: "port 5432", want: "port <redacted>"},
		{in: "cannot construct !!str `hunter2...` into struct", want: "cannot construct !!str `<redacted>` into struct"},
		{in: "line VEVTVC1PTkxZIGZpeHR1cmUga2V5IGZvciB0aGUgcGljb2xldCBTT1BTIGNvcnB1cw== of the key", want: "line <redacted> of the key"},
		{in: "whole " + strconv.Quote(string(pem)), want: "whole \"<redacted>\""},
		// Values under four characters stay: they would blank common words.
		{in: "user app", want: "user app"},
		{in: "nothing secret here", want: "nothing secret here"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, r.Scrub(tc.in), tc.in)
	}
}

func TestReaderScrubWithoutDecryption(t *testing.T) {
	t.Parallel()
	var none *sops.Reader
	assert.Equal(t, "hunter2-test", none.Scrub("hunter2-test"))
	assert.Equal(t, "hunter2-test", corpusReader(t, "srv-1").Scrub("hunter2-test"))
}
