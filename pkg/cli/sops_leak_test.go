package cli

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixturePlaintexts are the values of testdata/sops/plain/values.yaml long
// enough to be recognisable in an error.
var fixturePlaintexts = []string{
	"hunter2-test", "alpha-token", "beta-token", "gamma-secret", "srv1-pass",
	"slash-key", "tilde-key", "5432", "0.75", "visible",
}

// hexRun matches a run of hex digits as long as a truncated hash.
var hexRun = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)

// assertNoPlaintext fails when err carries a fixture plaintext or anything
// that looks like a hash of one.
func assertNoPlaintext(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	for _, p := range fixturePlaintexts {
		require.NotContains(t, msg, p, "error leaks a fixture plaintext")
	}
	require.Empty(t, hexRun.FindAllString(msg, -1), "error carries a hash-like hex run")
}
