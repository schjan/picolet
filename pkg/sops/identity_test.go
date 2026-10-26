package sops_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

func TestParseIdentitiesRecipients(t *testing.T) {
	t.Parallel()
	ids := testsops.Identities(t, "srv-1", "srv-pq", "srv-1")
	require.Equal(t, 3, ids.Len())

	var got []string
	for _, r := range ids.Recipients() {
		got = append(got, r.String())
	}
	require.Equal(t, []string{testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-pq")}, got)
	require.Equal(t, sops.RecipientHybrid, ids.Recipients()[1].Type())
}

func TestParseIdentitiesRejects(t *testing.T) {
	t.Parallel()
	key := testsops.IdentityFile(t, "srv-1")
	_, secretPart, ok := strings.Cut(string(key), "AGE-SECRET-KEY-1")
	require.True(t, ok)
	secretPart = strings.TrimSpace(secretPart)

	tests := []struct {
		name     string
		data     string
		want     error
		contains string
	}{
		{name: "empty file", data: "", want: sops.ErrNoIdentity},
		{name: "comments only", data: "# nothing here\n\n", want: sops.ErrNoIdentity},
		{
			name: "plugin identity", data: "AGE-PLUGIN-YUBIKEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ\n",
			want: sops.ErrBadIdentity, contains: "plugin",
		},
		{
			name: "SSH private key", data: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
			want: sops.ErrBadIdentity, contains: "SSH",
		},
		{
			name: "a recipient instead of an identity", data: testsops.Recipient(t, "srv-1") + "\n",
			want: sops.ErrBadIdentity, contains: "line 1",
		},
		{
			// A corrupted key must not be echoed.
			name: "malformed key after a good one", data: string(key) + "AGE-SECRET-KEY-1" + secretPart[:20] + "\n",
			want: sops.ErrBadIdentity, contains: "line 5",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := sops.ParseIdentities([]byte(tc.data))
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.contains)
			require.NotContains(t, err.Error(), secretPart[:12])
		})
	}
}
