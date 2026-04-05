//go:build onepassword_integration

package onepassword

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveIntegration(t *testing.T) {
	t.Parallel()
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	ref := os.Getenv("OP_TEST_SECRET_REF")
	if ref == "" {
		t.Skip("OP_TEST_SECRET_REF not set")
	}

	ctx := t.Context()
	client, err := NewClient(ctx, token)
	require.NoError(t, err)

	val, err := client.Resolve(ctx, ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveSecretSectionedField(t *testing.T) {
	t.Parallel()
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	ref := os.Getenv("OP_TEST_SECTIONED_REF")
	if ref == "" {
		t.Skip("OP_TEST_SECTIONED_REF not set")
	}

	ctx := t.Context()
	client, err := NewClient(ctx, token)
	require.NoError(t, err)

	val, err := client.Resolve(ctx, ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveAllIntegration(t *testing.T) {
	t.Parallel()
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	ref := os.Getenv("OP_TEST_SECRET_REF")
	if ref == "" {
		t.Skip("OP_TEST_SECRET_REF not set")
	}

	ctx := t.Context()
	client, err := NewClient(ctx, token)
	require.NoError(t, err)

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
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	ref := os.Getenv("OP_TEST_SECRET_REF")
	if ref == "" {
		t.Skip("OP_TEST_SECRET_REF not set")
	}

	ctx := t.Context()
	client, err := NewClient(ctx, token)
	require.NoError(t, err)

	badRef := "op://nonexistent-vault/nonexistent-item/field"
	results, err := client.ResolveAll(ctx, []string{ref, badRef})
	// Should have an error for the bad ref but still return the good ref.
	require.Error(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])
	require.NotContains(t, results, badRef)
}
