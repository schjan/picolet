//go:build onepassword_integration

package onepassword

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestClient creates a Client from the OP_SERVICE_ACCOUNT_TOKEN env var.
// Skips the test when the token is not set.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	client, err := NewClient(t.Context(), token)
	require.NoError(t, err)
	return client
}

// requireEnv returns the value of the given env var or skips the test.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	val := os.Getenv(key)
	if val == "" {
		t.Skipf("%s not set", key)
	}
	return val
}

func TestResolveIntegration(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECRET_REF")

	results, err := client.ResolveAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.NotEmpty(t, results[ref])
}

func TestResolveSecretSectionedField(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECTIONED_REF")

	results, err := client.ResolveAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.NotEmpty(t, results[ref])
}

func TestResolveAllIntegration(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECRET_REF")

	results, err := client.ResolveAll(t.Context(), []string{ref})
	require.NoError(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])
}

func TestResolveAllPartialFailureIntegration(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECRET_REF")

	badRef := "op://nonexistent-vault/nonexistent-item/field"
	results, err := client.ResolveAll(t.Context(), []string{ref, badRef})
	// Should have an error for the bad ref but still return the good ref.
	require.Error(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])
	require.NotContains(t, results, badRef)
}
