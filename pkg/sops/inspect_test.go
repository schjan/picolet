package sops_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

func inspectCorpus(t *testing.T, name string) *sops.File {
	t.Helper()
	f, err := sops.Inspect(name, corpusFile(t, name))
	require.NoError(t, err)
	return f
}

func recipients(t *testing.T, names ...string) []sops.Recipient {
	t.Helper()
	out := make([]sops.Recipient, 0, len(names))
	for _, n := range names {
		r, err := sops.ParseRecipient(corpusRecipient(t, n))
		require.NoError(t, err)
		out = append(out, r)
	}
	return out
}

func TestInspectReportsRecipients(t *testing.T) {
	t.Parallel()

	f := inspectCorpus(t, "values.sops.yaml")
	assert.True(t, f.IsSOPS)
	assert.Equal(t, sops.FormatYAML, f.Format)
	require.Len(t, f.KeyGroups, 1)
	assert.Equal(t, recipients(t, "srv-1", "srv-2", "operator-1"), f.KeyGroups[0].Age)
	assert.Equal(t, 1, f.RequiredGroups())
}

func TestInspectReportsKeyGroupsAndThreshold(t *testing.T) {
	t.Parallel()

	f := inspectCorpus(t, "key-groups.sops.yaml")
	require.True(t, f.IsSOPS)
	require.Len(t, f.KeyGroups, 3)
	assert.Equal(t, recipients(t, "srv-1", "operator-1"), f.KeyGroups[0].Age)
	assert.Equal(t, recipients(t, "srv-2"), f.KeyGroups[1].Age)
	assert.Equal(t, recipients(t, "srv-pq"), f.KeyGroups[2].Age)
	assert.Equal(t, 2, f.ShamirThreshold)
	assert.Equal(t, 2, f.RequiredGroups())
}

func TestInspectReportsNonAgeKeysSeparately(t *testing.T) {
	t.Parallel()

	f := inspectCorpus(t, "non-age.sops.yaml")
	require.True(t, f.IsSOPS)
	require.Len(t, f.KeyGroups, 1)
	assert.Equal(t, recipients(t, "srv-1", "srv-2"), f.KeyGroups[0].Age)
	assert.ElementsMatch(t, []string{"kms", "pgp"}, f.KeyGroups[0].Other)
}

func TestInspectNotSOPS(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"plaintext.sops.yaml": "no sops metadata",
		"no-mac.sops.yaml":    "sops.mac missing",
	}
	for name, reason := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := inspectCorpus(t, name)
			assert.False(t, f.IsSOPS)
			assert.Equal(t, reason, f.NotSOPSReason)
		})
	}
}

func TestInspectUnparsableFileIsNotSOPSAndNeverEchoesContent(t *testing.T) {
	t.Parallel()

	f, err := sops.Inspect("secrets/db.sops.json", []byte(`{"password": "hunter2-test"`))
	require.NoError(t, err)
	assert.False(t, f.IsSOPS)
	assert.NotContains(t, f.NotSOPSReason, "hunter2")
}

func TestInspectNeverEchoesAnAgeIdentityAsRecipient(t *testing.T) {
	t.Parallel()

	identity := zeroIdentity
	data := bytes.Replace(corpusFile(t, "values.sops.yaml"),
		[]byte("recipient: "+corpusRecipient(t, "srv-2")), []byte("recipient: "+identity), 1)
	f, err := sops.Inspect("values.sops.yaml", data)
	require.NoError(t, err)
	require.True(t, f.IsSOPS)
	require.Len(t, f.KeyGroups, 1)
	require.Len(t, f.KeyGroups[0].UnsupportedAge, 1)
	assert.NotContains(t, f.KeyGroups[0].UnsupportedAge[0], "QQQQ")
	out, err := json.Marshal(f)
	require.NoError(t, err)
	assert.NotContains(t, string(out), identity[len("AGE-SECRET-KEY-"):])
}

func TestInspectSizeCap(t *testing.T) {
	t.Parallel()

	big := bytes.Repeat([]byte("a"), sops.MaxFileSize+1)
	_, err := sops.Inspect("big.sops.yaml", big)
	require.ErrorIs(t, err, sops.ErrFileTooLarge)
	assert.Contains(t, err.Error(), "big.sops.yaml")

	atCap := append([]byte("k: "), bytes.Repeat([]byte("a"), sops.MaxFileSize-3)...)
	_, err = sops.Inspect("cap.sops.yaml", atCap)
	require.NoError(t, err, "exactly 1 MiB is allowed")
}

func TestReadFileSizeCap(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"secrets/big.sops.yaml": {Data: bytes.Repeat([]byte("a"), sops.MaxFileSize+1)},
		"secrets/ok.sops.yaml":  {Data: []byte("k: v\n")},
	}
	_, err := sops.ReadFile(fsys, "secrets/big.sops.yaml")
	require.ErrorIs(t, err, sops.ErrFileTooLarge)
	assert.Contains(t, err.Error(), "secrets/big.sops.yaml")

	data, err := sops.ReadFile(fsys, "secrets/ok.sops.yaml")
	require.NoError(t, err)
	assert.Equal(t, "k: v\n", string(data))
}

func TestLookup(t *testing.T) {
	t.Parallel()

	leaf := func(enc bool, typ string) sops.Node {
		return sops.Node{Kind: sops.NodeLeaf, Encrypted: enc, Type: typ}
	}
	cases := []struct {
		file, ptr string
		want      sops.Node
	}{
		{"values.sops.yaml", "/db/password", leaf(true, "str")},
		{"values.sops.yaml", "/db/port", leaf(true, "int")},
		{"values.sops.yaml", "/db/tls", leaf(true, "bool")},
		{"values.sops.yaml", "/db/ratio", leaf(true, "float")},
		{"values.sops.yaml", "/db/empty", leaf(false, "str")},
		{"values.sops.yaml", "/db/nothing", leaf(false, "null")},
		{"values.sops.yaml", "/tokens/1", leaf(true, "str")},
		{"values.sops.yaml", "/tokens/2/value", leaf(true, "str")},
		{"values.sops.yaml", "/hosts/srv-1.example.net/password", leaf(true, "str")},
		{"values.sops.yaml", "/odd_keys/a~1b", leaf(true, "str")},
		{"values.sops.yaml", "/odd_keys/m~0n", leaf(true, "str")},
		{"values.sops.yaml", "/note_unencrypted", leaf(false, "str")},
		{"values.sops.yaml", "/db", sops.Node{Kind: sops.NodeMap}},
		{"values.sops.yaml", "/tokens", sops.Node{Kind: sops.NodeList}},
		{"values.sops.yaml", "/tokens/2", sops.Node{Kind: sops.NodeMap}},
		{"encrypted-regex.sops.yaml", "/db/user", leaf(false, "str")},
		{"encrypted-regex.sops.yaml", "/db/password", leaf(true, "str")},
		{"tampered-type.sops.yaml", "/db/password", leaf(true, "int")},
		{"values.sops.json", "/db/port", leaf(true, "int")},
		{"values.sops.json", "/tokens/0", leaf(true, "str")},
		{"values.sops.env", "/DB_PASSWORD", leaf(true, "str")},
		{"values.sops.env", "/DB_PORT", leaf(true, "str")},
		{"values.sops.ini", "/db/password", leaf(true, "str")},
		{"blob.sops.bin", "/data", leaf(true, "str")},
	}
	for _, tc := range cases {
		t.Run(tc.file+"#"+tc.ptr, func(t *testing.T) {
			t.Parallel()
			f := inspectCorpus(t, tc.file)
			p, err := sops.ParsePointer(tc.ptr)
			require.NoError(t, err)
			got, err := f.Lookup(p)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLookupMissingPath(t *testing.T) {
	t.Parallel()

	f := inspectCorpus(t, "values.sops.yaml")
	cases := map[string]string{
		"/db/missing":        `"missing" not found`,
		"/tokens/3":          "out of range",
		"/tokens/x":          "not a list index",
		"/tokens/01":         "not a list index",
		"/tokens/-":          "not a list index",
		"/db/password/inner": "is a leaf",
		"/sops":              `"sops" not found`, // metadata is not data
	}
	for ptr, want := range cases {
		t.Run(ptr, func(t *testing.T) {
			t.Parallel()
			p, err := sops.ParsePointer(ptr)
			require.NoError(t, err)
			_, err = f.Lookup(p)
			require.ErrorIs(t, err, sops.ErrBadPointer)
			assert.Contains(t, err.Error(), want)
			assertNoPlaintext(t, err)
		})
	}
}

func TestLookupRootIsTheDocument(t *testing.T) {
	t.Parallel()

	got, err := inspectCorpus(t, "values.sops.yaml").Lookup(sops.Pointer{})
	require.NoError(t, err)
	assert.Equal(t, sops.NodeMap, got.Kind)
}

func TestLookupOnNonSOPSFile(t *testing.T) {
	t.Parallel()

	p, err := sops.ParsePointer("/db/password")
	require.NoError(t, err)
	_, err = inspectCorpus(t, "plaintext.sops.yaml").Lookup(p)
	require.ErrorIs(t, err, sops.ErrNotSOPSFile)
	assertNoPlaintext(t, err)
}
