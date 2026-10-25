package sops_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// The keyless reader (validate) never fails a per-ref read the decrypting
// reader (the Agent) accepts: for every corpus file that decrypts, every ref
// the Agent reads, as a string, as data or as a direct secrets: entry, also
// passes keyless for a Host that declares the identities' recipients. Where
// the keyless value is not a placeholder (a plaintext leaf), it is the
// Agent's value; a typed placeholder has the type of the Agent's value.
func TestKeylessAcceptsWhatDecryptingAccepts(t *testing.T) {
	t.Parallel()
	for _, tc := range decryptCases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			dec := corpusReader(t, tc.ids...)
			keyless := sops.NewKeylessReader(os.DirFS(corpusDir), keylessHost(t, "srv", tc.ids...))
			whole := "sops://corpus/" + tc.file
			refs := []string{whole}
			for _, p := range tc.pointers {
				refs = append(refs, whole+"#"+p)
			}

			want, err := dec.ReadAll(t.Context(), refs)
			require.NoError(t, err)
			got, err := keyless.ReadAll(t.Context(), refs)
			require.NoError(t, err, "keyless readSopsSecret fails a ref the Agent accepts")
			var direct []string
			for _, ref := range refs {
				assertKeylessValue(t, want[ref], got[ref], ref)
				if want[ref] != "" {
					direct = append(direct, ref)
				}
			}

			gotDirect, err := keyless.ReadDirect(t.Context(), direct)
			require.NoError(t, err, "keyless direct secrets: fails a ref the Agent accepts")
			for _, ref := range direct {
				assertKeylessValue(t, want[ref], gotDirect[ref], ref)
			}

			for _, ref := range refs {
				wantData, err := dec.ReadData(t.Context(), ref)
				require.NoError(t, err, ref)
				gotData, err := keyless.ReadData(t.Context(), ref)
				require.NoError(t, err, "keyless readSopsData fails %s, which the Agent accepts", ref)
				assertKeylessData(t, wantData, gotData, ref)
			}
		})
	}
}

// assertKeylessValue fails unless got (keyless) is the decrypted value
// want, the string placeholder, or a typed placeholder (0, 0.0, false, the
// zero time) that want parses as the same type as.
func assertKeylessValue(t *testing.T, want, got, at string) {
	t.Helper()
	var err error
	switch got {
	case want, sops.Placeholder:
		return
	case "0":
		_, err = strconv.Atoi(want)
	case "0.0":
		_, err = strconv.ParseFloat(want, 64)
	case "false":
		_, err = strconv.ParseBool(want)
	case "0001-01-01T00:00:00Z":
		_, err = time.Parse(time.RFC3339, want)
	default:
		assert.Equal(t, want, got, at)
		return
	}
	assert.NoError(t, err, "%s: typed placeholder %q for a decrypted value of another type", at, got)
}

// assertKeylessData compares a keyless readSopsData result with the
// decrypted one: the same structure, every leaf the placeholder or the
// decrypted value.
func assertKeylessData(t *testing.T, want, got any, at string) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		require.True(t, ok, "%s: keyless returned %T for a map", at, got)
		require.ElementsMatch(t, keysOf(w), keysOf(g), at)
		for k := range w {
			assertKeylessData(t, w[k], g[k], at+"/"+k)
		}
	case []any:
		g, ok := got.([]any)
		require.True(t, ok, "%s: keyless returned %T for a list", at, got)
		require.Len(t, g, len(w), at)
		for i := range w {
			assertKeylessData(t, w[i], g[i], at)
		}
	default:
		s, ok := got.(string)
		require.True(t, ok, "%s: keyless returned %T for a leaf", at, got)
		ws, ok := want.(string)
		require.True(t, ok, "%s: decrypted leaf is %T", at, want)
		assertKeylessValue(t, ws, s, at)
	}
}

// Keyless, a plaintext leaf (one the file's encryption rules leave
// unencrypted, "" and null included) renders as the Agent renders it; only
// an encrypted leaf renders as the placeholder.
func TestKeylessPlaintextLeaves(t *testing.T) {
	t.Parallel()
	p := sops.Placeholder
	r := sops.NewKeylessReader(os.DirFS(corpusDir), hostSrv1(t))
	file := "sops://corpus/encrypted-regex.sops.yaml" // encrypts only password and value keys
	strings := map[string]string{
		file + "#/db/password":      p,
		file + "#/db/user":          "app",
		file + "#/db/port":          "5432",
		file + "#/db/tls":           "true",
		file + "#/db/empty":         "",
		file + "#/db/nothing":       "",
		file + "#/note_unencrypted": "visible",
		file:                        p,
	}
	for ref, want := range strings {
		got, err := r.ReadAll(t.Context(), []string{ref})
		require.NoError(t, err)
		assert.Equal(t, want, got[ref], ref)
	}

	got, err := r.ReadDirect(t.Context(), []string{file + "#/db/port"})
	require.NoError(t, err)
	assert.Equal(t, "5432", got[file+"#/db/port"])

	data, err := r.ReadData(t.Context(), file+"#/db")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"user": "app", "password": p, "port": "5432", "tls": "true", "ratio": "0.75", "empty": "", "nothing": "",
	}, data)
}

// Whether a leaf is encrypted is the file's encryption rules' call, as in
// the Agent's decrypt: a value that only looks like SOPS ciphertext (here a
// bogus type tag) under a key the rules leave in plaintext is a plaintext
// value. mac_only_encrypted keeps such a value out of the MAC, so the Agent
// accepts the file; keyless accepts it too and renders the literal.
func TestKeylessCiphertextLookalikeInPlaintextPosition(t *testing.T) {
	t.Parallel()
	lookalike := "ENC[AES256_GCM,data:AAAA,iv:AAAA,tag:AAAA,type:nope]"
	data := strings.Replace(string(corpusFile(t, "mac-only-encrypted.sops.yaml")),
		"note_unencrypted: visible", "note_unencrypted: "+lookalike, 1)
	fsys := fstest.MapFS{"db.sops.yaml": {Data: []byte(data)}}
	ref := "sops://db.sops.yaml#/note_unencrypted"

	want, err := sops.NewReader(fsys, corpusIdentities(t, "srv-1")).ReadAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.Equal(t, lookalike, want[ref])

	got, err := sops.NewKeylessReader(fsys, hostSrv1(t)).ReadAll(t.Context(), []string{ref})
	require.NoError(t, err)
	assert.Equal(t, lookalike, got[ref])
}

// A plaintext value where the file's rules require ciphertext (an edit
// outside sops) fails the Agent's decrypt for the whole file; keyless fails
// every ref into that file the same way, naming the value's pointer and
// never quoting it.
func TestKeylessPlaintextInEncryptedPosition(t *testing.T) {
	t.Parallel()
	data := regexp.MustCompile(`(?m)^(    password: )ENC\[[^\]]*\]`).
		ReplaceAllString(string(corpusFile(t, "values.sops.yaml")), "${1}hunter2-test")
	fsys := fstest.MapFS{"db.sops.yaml": {Data: []byte(data)}}
	refs := []string{"sops://db.sops.yaml#/db/password", "sops://db.sops.yaml#/db/user"}

	_, err := sops.NewReader(fsys, corpusIdentities(t, "srv-1")).ReadAll(t.Context(), refs)
	require.ErrorIs(t, err, sops.ErrEditedOutsideSOPS)
	assertNoPlaintext(t, err)

	r := sops.NewKeylessReader(fsys, hostSrv1(t))
	_, err = r.ReadAll(t.Context(), refs)
	require.ErrorIs(t, err, sops.ErrEditedOutsideSOPS)
	for _, ref := range refs {
		require.ErrorContains(t, err, ref)
	}
	require.ErrorContains(t, err, `"/db/password" is not encrypted`)
	assertNoPlaintext(t, err)

	_, err = r.ReadData(t.Context(), "sops://db.sops.yaml#/tokens")
	require.ErrorIs(t, err, sops.ErrEditedOutsideSOPS)
	assertNoPlaintext(t, err)
}
