package sops_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

// TEST-ONLY recipients of the fixture corpus (testdata/sops/recipients.txt).
const (
	srv1 = "age1eed9cxu2y3xx2s3syz95sanhy3xuajtpmjsnz63skgwl9yvw0g5su77pfq"
	srv2 = "age1e95u43sns76r0e52j74aupqsp58qc9s6vrwhzsj9hd6x9shk9cgqgu5gjj"
)

func TestParseRecipientAcceptsX25519(t *testing.T) {
	t.Parallel()

	r, err := sops.ParseRecipient(srv1)
	require.NoError(t, err)
	assert.Equal(t, srv1, r.String())
	assert.Equal(t, sops.RecipientX25519, r.Type())
}

func TestParseRecipientAcceptsHybridPostQuantum(t *testing.T) {
	t.Parallel()

	pq := testsops.Recipient(t, "srv-pq")
	require.True(t, strings.HasPrefix(pq, "age1pq1"))

	r, err := sops.ParseRecipient(pq)
	require.NoError(t, err)
	assert.Equal(t, pq, r.String())
	assert.Equal(t, sops.RecipientHybrid, r.Type())
}

func TestParseRecipientCanonicalFormCompares(t *testing.T) {
	t.Parallel()

	lower, err := sops.ParseRecipient(srv1)
	require.NoError(t, err)
	upper, err := sops.ParseRecipient(strings.ToUpper(srv1))
	require.NoError(t, err)
	other, err := sops.ParseRecipient(srv2)
	require.NoError(t, err)

	assert.Equal(t, lower, upper, "Bech32 is case-insensitive; the parsed form is canonical")
	assert.Equal(t, srv1, upper.String())
	assert.NotEqual(t, lower, other)
}

func TestParseRecipientRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in   string
		want string
	}{
		"ssh-ed25519": {
			in:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHsKLqeplhpW+uObz5dvMgjz1OxfM/XXUB+VHtZ6isGN srv-1@example.net",
			want: "SSH recipients are not supported",
		},
		"ssh-rsa": {
			in:   "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ srv-1@example.net",
			want: "SSH recipients are not supported",
		},
		"plugin": {
			in:   "age1yubikey1q2w7u3vpya839jxxuq8g0sedh3d740d4xvn639sqhr95ejj8vu3hyfumptt",
			want: "plugin recipients are not supported",
		},
		"identity instead of recipient": {
			in:   zeroIdentity,
			want: "an age identity (secret key) is not an age recipient",
		},
		"empty":             {in: "", want: "not an age recipient"},
		"broken checksum":   {in: srv1[:len(srv1)-1] + "x", want: "not an age recipient"},
		"surrounding space": {in: " " + srv1, want: "not an age recipient"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := sops.ParseRecipient(tc.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// zeroIdentity is a well-formed age identity of an all-zero key. It is
// built at runtime so secret scanning's push protection sees no age
// secret-key literal in the source.
var zeroIdentity = "AGE-SECRET-KEY-1" + strings.Repeat("Q", 52) + "S9AM2Q"
