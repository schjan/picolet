package protonpass

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeStderr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "short message preserved",
			in:   "session expired",
			want: "session expired",
		},
		{
			name: "long alphanumeric token redacted",
			in:   "got: pst_abcdefghijklmnopqrstuvwxyz0123456789==",
			// pst is 3 chars, separator _, then 36-char alnum body, then ==
			want: "got: pst_<redacted>==",
		},
		{
			name: "short alphanumeric run preserved",
			in:   "short ABC123 fine",
			want: "short ABC123 fine",
		},
		{
			name: "multiple tokens redacted",
			in:   "key1=AAAAbbbbCCCCddddEEEEffff key2=zzzzyyyyxxxxwwwwvvvvvvvv",
			want: "key1=<redacted> key2=<redacted>",
		},
		{
			name: "mixed safe and token",
			in:   "error: invalid token aGVsbG93b3JsZGFiY2RlZmdoaWprbA==",
			want: "error: invalid token <redacted>==",
		},
		{
			name: "leading and trailing whitespace trimmed",
			in:   "  session not found\n",
			want: "session not found",
		},
		{
			name: "very long output truncated",
			in:   strings.Repeat("error message with details. ", 50),
			want: strings.Repeat("error message with details. ", 50)[:maxSanitizedStderrBytes] + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeStderr([]byte(tt.in))
			assert.Equal(t, tt.want, got)
		})
	}
}
