package onepassword

import (
	"context"
	"testing"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockSecretsAPI struct {
	resolveAllFn func(ctx context.Context, refs []string) (op.ResolveAllResponse, error)
}

func (m *mockSecretsAPI) Resolve(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockSecretsAPI) ResolveAll(ctx context.Context, refs []string) (op.ResolveAllResponse, error) {
	return m.resolveAllFn(ctx, refs)
}

func TestResolveAllPartialFailure(t *testing.T) {
	t.Parallel()

	goodRef := "op://vault/good/field"
	badRef := "op://vault/bad/field"

	mock := &mockSecretsAPI{
		resolveAllFn: func(_ context.Context, _ []string) (op.ResolveAllResponse, error) {
			goodResult := op.ResolvedReference{Secret: "good-value"}
			badErr := op.NewResolveReferenceErrorTypeVariantFieldNotFound()
			return op.ResolveAllResponse{
				IndividualResponses: map[string]op.Response[op.ResolvedReference, op.ResolveReferenceError]{
					goodRef: {Content: &goodResult},
					badRef:  {Error: &badErr},
				},
			}, nil
		},
	}
	client := &Client{secrets: mock}

	results, err := client.ResolveAll(t.Context(), []string{goodRef, badRef})
	require.ErrorContains(t, err, badRef)
	require.ErrorContains(t, err, "fieldNotFound")

	// Partial results: good ref is present despite the error.
	assert.Equal(t, "good-value", results[goodRef])
	_, hasBad := results[badRef]
	assert.False(t, hasBad)
}

func TestResolveAllAllSucceed(t *testing.T) {
	t.Parallel()

	refs := []string{"op://vault/a/field", "op://vault/b/field"}
	mock := &mockSecretsAPI{
		resolveAllFn: func(_ context.Context, _ []string) (op.ResolveAllResponse, error) {
			aResult := op.ResolvedReference{Secret: "val-a"}
			bResult := op.ResolvedReference{Secret: "val-b"}
			return op.ResolveAllResponse{
				IndividualResponses: map[string]op.Response[op.ResolvedReference, op.ResolveReferenceError]{
					refs[0]: {Content: &aResult},
					refs[1]: {Content: &bResult},
				},
			}, nil
		},
	}
	client := &Client{secrets: mock}

	results, err := client.ResolveAll(t.Context(), refs)
	require.NoError(t, err)
	assert.Equal(t, "val-a", results[refs[0]])
	assert.Equal(t, "val-b", results[refs[1]])
}

func TestResolveAllEmpty(t *testing.T) {
	t.Parallel()
	client := &Client{secrets: &mockSecretsAPI{}}

	results, err := client.ResolveAll(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, results)
}
