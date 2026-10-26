package sops_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

// corpusReader returns a Reader over testdata/sops, so refs read
// sops://corpus/<file>.
func corpusReader(t *testing.T, names ...string) *sops.Reader {
	t.Helper()
	return sops.NewReader(os.DirFS(corpusDir), testsops.Identities(t, names...))
}

// morePlaintexts are the values in testdata/sops/plain beyond values.yaml's
// (testsops.Plaintexts) long enough to be recognisable in an error. (The
// value "edited" of tampered-mac.sops.yaml is left out: it is part of the
// class name "edited outside sops".)
var morePlaintexts = []string{"VEVTVC1PTkxZIGZpeHR1cmUga2V5", "TEST-ONLY fixture values"}

func TestReaderSingleRef(t *testing.T) {
	t.Parallel()
	r := corpusReader(t, "srv-1")

	ref := "sops://corpus/values.sops.yaml#/db/password"
	got, err := r.ReadAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.Equal(t, map[string]string{ref: "hunter2-test"}, got)
}

func TestReaderBatch(t *testing.T) {
	t.Parallel()
	r := corpusReader(t, "srv-1")

	refs := []string{
		"sops://corpus/values.sops.yaml#/db/password",
		"sops://corpus/values.sops.yaml#/tokens/1",
		"sops://corpus/values.sops.yaml#/hosts/srv-1.example.net/password",
		"sops://corpus/values.sops.json#/tokens/0",
		"sops://corpus/values.sops.env#/DB_PASSWORD",
	}
	got, err := r.ReadAll(t.Context(), refs)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		refs[0]: "hunter2-test",
		refs[1]: "beta-token",
		refs[2]: "srv1-pass",
		refs[3]: "alpha-token",
		refs[4]: "hunter2-test",
	}, got)
}

func TestReaderPartialFailure(t *testing.T) {
	t.Parallel()
	r := corpusReader(t, "srv-1")

	good := "sops://corpus/values.sops.yaml#/db/password"
	missing := "sops://corpus/values.sops.yaml#/db/nope"
	otherFile := "sops://corpus/tls.sops.pem"
	got, err := r.ReadAll(t.Context(), []string{good, missing, otherFile})
	require.ErrorIs(t, err, sops.ErrBadPointer)
	require.ErrorContains(t, err, missing)
	require.NotContains(t, err.Error(), good)
	require.Equal(t, "hunter2-test", got[good])
	require.Contains(t, got[otherFile], "BEGIN TEST-ONLY KEY")
	require.NotContains(t, got, missing)
	testsops.AssertNoPlaintext(t, err, morePlaintexts...)
}

var yamlPointers = []string{
	"/db/user", "/db/password", "/db/port", "/db/tls", "/db/ratio", "/db/empty", "/db/nothing",
	"/tokens/0", "/tokens/1", "/tokens/2/name", "/tokens/2/value",
	"/hosts/srv-1.example.net/password", "/odd_keys/a~1b", "/odd_keys/m~0n", "/note_unencrypted",
}

// decryptCases are the corpus files that decrypt, with the identities that
// open them and the pointers rendered into their golden file.
var decryptCases = []struct {
	file     string
	ids      []string
	pointers []string
}{
	{file: "values.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
	{file: "values-reencrypted.sops.yaml", ids: []string{"srv-2"}, pointers: yamlPointers},
	{file: "values.sops.json", ids: []string{"srv-2"}, pointers: []string{
		"/db/user", "/db/password", "/db/port", "/db/tls", "/db/empty", "/db/nothing",
		"/tokens/0", "/tokens/1", "/note_unencrypted",
	}},
	{file: "values.sops.env", ids: []string{"srv-1"}, pointers: []string{
		"/DB_PASSWORD", "/API_TOKEN", "/DB_PORT", "/NOTE_unencrypted",
	}},
	{file: "values.sops.ini", ids: []string{"srv-1"}, pointers: []string{
		"/db/user", "/db/password", "/api/token",
	}},
	{file: "blob.sops.bin", ids: []string{"srv-1"}, pointers: []string{"/data"}},
	{file: "tls.sops.pem", ids: []string{"srv-1"}, pointers: []string{"/data"}},
	{file: "unencrypted-suffix.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
	{file: "encrypted-regex.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
	{file: "mac-only-encrypted.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
	// Key groups: srv-1 and srv-2 open two of three groups, the threshold.
	{file: "key-groups.sops.yaml", ids: []string{"srv-1", "srv-2"}, pointers: yamlPointers},
	// Hybrid post-quantum identity.
	{file: "pq.sops.yaml", ids: []string{"srv-pq"}, pointers: yamlPointers},
	// KMS and PGP stanzas next to age: ignored, never contacted.
	{file: "non-age.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
	// Rotation: a two-identity Host whose new key (srv-3) is in no file yet.
	{file: "base-tamper.sops.yaml", ids: []string{"srv-3", "srv-1"}, pointers: yamlPointers},
	// srv-2's stanza is labelled srv-3; srv-1's is untouched and still opens.
	// (With srv-2 or srv-3 it fails: see errorClassCases.)
	{file: "tampered-recipient.sops.yaml", ids: []string{"srv-1"}, pointers: yamlPointers},
}

// TestReaderDecryptGolden snapshots the decrypted output of every corpus file
// that decrypts, whole and per pointer, so a SOPS upgrade that changes the
// rendering shows up as a golden diff. Refresh with -update.
func TestReaderDecryptGolden(t *testing.T) {
	t.Parallel()
	g := goldie.New(t, goldie.WithFixtureDir(filepath.Join("testdata", "decrypt")))
	for _, tc := range decryptCases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			r := corpusReader(t, tc.ids...)
			whole := "sops://corpus/" + tc.file
			refs := []string{whole}
			for _, p := range tc.pointers {
				refs = append(refs, whole+"#"+p)
			}
			got, err := r.ReadAll(t.Context(), refs)
			require.NoError(t, err)

			var b strings.Builder
			for _, ref := range refs {
				_, frag, _ := strings.Cut(ref, "#")
				if frag == "" {
					frag = "(whole file)"
				}
				fmt.Fprintf(&b, "=== %s ===\n%s\n", frag, got[ref])
			}
			g.Assert(t, tc.file, []byte(b.String()))
		})
	}
}

// oversizedFS holds a *.sops.* file one byte over MaxFileSize.
var oversizedFS = fstest.MapFS{"big.sops.yaml": &fstest.MapFile{
	Data: append([]byte("db:\n    password: "), make([]byte, sops.MaxFileSize)...),
}}

// errorClassCases holds one ref per error class (and per way into it).
var errorClassCases = []struct {
	name string
	ref  string
	ids  []string // nil: no identity configured
	fsys fs.FS    // nil: the corpus
	want error
	// contains are substrings the message must carry.
	contains []string
}{
	{
		name: "no identity configured", ref: "sops://corpus/values.sops.yaml#/db/password",
		want: sops.ErrNoIdentity, contains: []string{"sops.age_identity_path"},
	},
	{
		name: "not a recipient", ref: "sops://corpus/values.sops.env#/DB_PASSWORD", ids: []string{"srv-2"},
		want: sops.ErrNotRecipient,
		contains: []string{
			"sops://corpus/values.sops.env#/DB_PASSWORD", "corpus/values.sops.env",
			testsops.RecipientOf("srv-2"), ".sops.yaml", "sops updatekeys corpus/values.sops.env",
		},
	},
	{
		name: "not a recipient with either rotation key", ref: "sops://corpus/values.sops.env#/DB_PASSWORD",
		ids: []string{"srv-2", "srv-3"}, want: sops.ErrNotRecipient,
		contains: []string{testsops.RecipientOf("srv-2"), testsops.RecipientOf("srv-3")},
	},
	{
		name: "not a recipient in enough key groups", ref: "sops://corpus/key-groups.sops.yaml#/db/password",
		ids: []string{"srv-1"}, want: sops.ErrNotRecipient, contains: []string{"1 of the 2 key groups"},
	},
	{
		name: "no stanza for a post-quantum Host", ref: "sops://corpus/blob.sops.bin",
		ids: []string{"srv-pq"}, want: sops.ErrNotRecipient, contains: []string{testsops.RecipientOf("srv-pq")},
	},
	{
		name: "edited outside sops: MAC", ref: "sops://corpus/tampered-mac.sops.yaml#/db/password",
		ids: []string{"srv-1"}, want: sops.ErrEditedOutsideSOPS, contains: []string{"MAC"},
	},
	{
		name: "edited outside sops: ciphertext", ref: "sops://corpus/tampered-ciphertext.sops.yaml#/db/user",
		ids: []string{"srv-1"}, want: sops.ErrEditedOutsideSOPS,
	},
	{
		name: "edited outside sops: type tag", ref: "sops://corpus/tampered-type.sops.yaml#/db/user",
		ids: []string{"srv-1"}, want: sops.ErrEditedOutsideSOPS,
	},
	{
		// The label names srv-3, so the pre-check passes, but the stanza is
		// srv-2's: the decrypt result decides.
		name: "edited outside sops: relabelled stanza", ref: "sops://corpus/tampered-recipient.sops.yaml#/db/user",
		ids: []string{"srv-3"}, want: sops.ErrEditedOutsideSOPS,
	},
	{
		// The pre-check runs before decrypting: srv-2's stanza would open,
		// but no label names srv-2, so it is not a recipient.
		name: "not a recipient: stanza relabelled away", ref: "sops://corpus/tampered-recipient.sops.yaml#/db/user",
		ids: []string{"srv-2"}, want: sops.ErrNotRecipient, contains: []string{testsops.RecipientOf("srv-2")},
	},
	{
		name: "bad pointer: no such key", ref: "sops://corpus/values.sops.yaml#/db/nope",
		ids: []string{"srv-1"}, want: sops.ErrBadPointer,
	},
	{
		name: "bad pointer: index out of range", ref: "sops://corpus/values.sops.yaml#/tokens/7",
		ids: []string{"srv-1"}, want: sops.ErrBadPointer,
	},
	{
		name: "bad pointer: below a leaf", ref: "sops://corpus/values.sops.yaml#/db/password/x",
		ids: []string{"srv-1"}, want: sops.ErrBadPointer,
	},
	{
		name: "bad pointer: malformed", ref: "sops://corpus/values.sops.yaml#db",
		ids: []string{"srv-1"}, want: sops.ErrBadPointer,
	},
	{
		name: "not a leaf: map", ref: "sops://corpus/values.sops.yaml#/db",
		ids: []string{"srv-1"}, want: sops.ErrNotLeaf, contains: []string{`"/db" is a map`},
	},
	{
		name: "not a leaf: list", ref: "sops://corpus/values.sops.yaml#/tokens",
		ids: []string{"srv-1"}, want: sops.ErrNotLeaf, contains: []string{`"/tokens" is a list`},
	},
	{
		name: "not a SOPS path", ref: "sops://plain/values.yaml#/db/password",
		ids: []string{"srv-1"}, want: sops.ErrNotSOPSPath,
	},
	{
		name: "not a SOPS file: plaintext", ref: "sops://corpus/plaintext.sops.yaml#/db/password",
		ids: []string{"srv-1"}, want: sops.ErrNotSOPSFile,
	},
	{
		name: "not a SOPS file: no MAC", ref: "sops://corpus/no-mac.sops.yaml#/db/password",
		ids: []string{"srv-1"}, want: sops.ErrNotSOPSFile,
	},
	{
		name: "file too large", ref: "sops://big.sops.yaml#/db/password",
		ids: []string{"srv-1"}, fsys: oversizedFS, want: sops.ErrFileTooLarge,
	},
}

func TestReaderErrorClasses(t *testing.T) {
	t.Parallel()
	for _, tc := range errorClassCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ids *sops.Identities
			if tc.ids != nil {
				ids = testsops.Identities(t, tc.ids...)
			}
			fsys := tc.fsys
			if fsys == nil {
				fsys = os.DirFS(corpusDir)
			}
			got, err := sops.NewReader(fsys, ids).ReadAll(t.Context(), []string{tc.ref})
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.ref)
			for _, s := range tc.contains {
				require.ErrorContains(t, err, s)
			}
			require.Empty(t, got)
			testsops.AssertNoPlaintext(t, err, morePlaintexts...)
		})
	}
}

// TestReaderFileErrorPerRef reads two refs into a file that fails: each ref
// carries the file's error.
func TestReaderFileErrorPerRef(t *testing.T) {
	t.Parallel()
	r := corpusReader(t, "srv-1")
	refs := []string{
		"sops://corpus/tampered-mac.sops.yaml#/db/user",
		"sops://corpus/tampered-mac.sops.yaml#/db/password",
	}
	got, err := r.ReadAll(t.Context(), refs)
	require.Empty(t, got)
	require.ErrorIs(t, err, sops.ErrEditedOutsideSOPS)
	for _, ref := range refs {
		require.ErrorContains(t, err, ref)
	}
	testsops.AssertNoPlaintext(t, err, morePlaintexts...)
}

// A scalar leaf that is not a string (int, bool, float) renders as
// `sops decrypt --extract` prints it minus exactly one trailing newline;
// string leaves and whole files stay byte-for-byte as sops prints them.
func TestReaderLeafNewlines(t *testing.T) {
	t.Parallel()
	refs := map[string]string{
		"sops://corpus/values.sops.yaml#/db/port":  "5432",
		"sops://corpus/values.sops.yaml#/db/tls":   "true",
		"sops://corpus/values.sops.yaml#/db/ratio": "0.75",
		"sops://corpus/values.sops.json#/db/port":  "5432",
		"sops://corpus/tls.sops.pem#/data":         "-----BEGIN TEST-ONLY KEY-----\nVEVTVC1PTkxZIGZpeHR1cmUga2V5IGZvciB0aGUgcGljb2xldCBTT1BTIGNvcnB1cw==\n-----END TEST-ONLY KEY-----\n",
	}
	r := corpusReader(t, "srv-1", "srv-2")
	for ref, want := range refs {
		got, err := r.ReadAll(t.Context(), []string{ref})
		require.NoError(t, err)
		assert.Equal(t, want, got[ref], ref)

		data, err := r.ReadData(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, want, data, "readSopsData renders %s like readSopsSecret", ref)
	}

	whole := "sops://corpus/values.sops.yaml"
	got, err := r.ReadAll(t.Context(), []string{whole})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got[whole], "note_unencrypted: visible\n"), "a whole file keeps its trailing newline")
}
