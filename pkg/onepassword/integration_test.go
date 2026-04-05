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

	val, err := client.Resolve(t.Context(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveSecretSectionedField(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECTIONED_REF")

	val, err := client.Resolve(t.Context(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveAllIntegration(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ref := requireEnv(t, "OP_TEST_SECRET_REF")

	ctx := t.Context()

	// Batch with a single valid ref — should return the same value as Resolve.
	results, err := client.ResolveAll(ctx, []string{ref})
	require.NoError(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])

	// Cross-check: batch result matches individual Resolve.
	individual, err := client.Resolve(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, individual, results[ref])
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
