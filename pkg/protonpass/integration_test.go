//go:build protonpass_integration

package protonpass

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireEnv returns the value of the given env var or skips the test.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	val := os.Getenv(key)
	if val == "" {
		t.Skipf("%s not set", key)
	}
	return val
}

// newIntegrationClient builds a Client wired with a real pass-cli binary
// and a PAT from env vars. SessionDir is isolated to t.TempDir so the test
// does not touch the developer's existing pass-cli session; pass-cli's
// filesystem-based local key provider generates a fresh local.key inside it.
func newIntegrationClient(t *testing.T) *Client {
	t.Helper()
	pat := requireEnv(t, "PP_PERSONAL_ACCESS_TOKEN")

	dir := t.TempDir()
	patPath := filepath.Join(dir, "pat")
	require.NoError(t, os.WriteFile(patPath, []byte(pat), 0o600))

	c, err := NewClient(ClientConfig{
		PATPath:    patPath,
		SessionDir: filepath.Join(dir, "session"),
	})
	require.NoError(t, err)
	return c
}

func TestEnsureSessionIntegration(t *testing.T) {
	t.Parallel()
	c := newIntegrationClient(t)
	require.NoError(t, c.EnsureSession(t.Context()))
}

func TestResolveIntegration(t *testing.T) {
	t.Parallel()
	c := newIntegrationClient(t)
	ref := requireEnv(t, "PP_TEST_REF")

	val, err := c.Resolve(t.Context(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveAllIntegration(t *testing.T) {
	t.Parallel()
	c := newIntegrationClient(t)
	ref := requireEnv(t, "PP_TEST_REF")

	results, err := c.ResolveAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])

	// Cross-check: batch result equals individual Resolve.
	individual, err := c.Resolve(t.Context(), ref)
	require.NoError(t, err)
	require.Equal(t, individual, results[ref])
}

func TestResolveAllPartialFailureIntegration(t *testing.T) {
	t.Parallel()
	c := newIntegrationClient(t)
	ref := requireEnv(t, "PP_TEST_REF")

	badRef := "pass://nonexistent-share/nonexistent-item/field"
	results, err := c.ResolveAll(t.Context(), []string{ref, badRef})
	require.Error(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])
	require.NotContains(t, results, badRef)
}
