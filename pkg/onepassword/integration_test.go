//go:build onepassword_integration

package onepassword

import (
	"context"
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

	ctx := context.Background()
	client, err := NewClient(ctx, token)
	require.NoError(t, err)

	val, err := client.Resolve(ctx, ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}
