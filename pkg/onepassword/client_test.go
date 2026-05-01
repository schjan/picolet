package onepassword

import (
	"context"
	"runtime"
	"testing"
	"time"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSecretsAPI is a minimal hand-written mock. If it grows more complex,
// consider switching to a mockery-generated mock (see .mockery.yaml).
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
	client := &Client{opClient: &op.Client{SecretsAPI: mock}}

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
	client := &Client{opClient: &op.Client{SecretsAPI: mock}}

	results, err := client.ResolveAll(t.Context(), refs)
	require.NoError(t, err)
	assert.Equal(t, "val-a", results[refs[0]])
	assert.Equal(t, "val-b", results[refs[1]])
}

func TestResolveAllEmpty(t *testing.T) {
	t.Parallel()
	client := &Client{opClient: &op.Client{SecretsAPI: &mockSecretsAPI{}}}

	results, err := client.ResolveAll(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, results)
}

// TestClientHasOpClientField is a compile-time structural assertion: if the
// opClient field is removed or retyped, this test file fails to compile.
// Guards against accidental removal of the lifetime-anchoring field, which
// would silently re-introduce the "invalid client id" production bug.
func TestClientHasOpClientField(t *testing.T) {
	t.Parallel()
	// The explicit *op.Client type is the assertion: if the field is renamed,
	// removed, or retyped, this line fails to compile.
	//nolint:staticcheck // QF1011: explicit type is intentional — this is a structural type assertion, not a regular declaration
	var _ *op.Client = new(Client).opClient
}

// TestClientRetainsSDKClient verifies that Client holds a strong reference to
// the underlying *op.Client, preventing the SDK's runtime finalizer from firing
// while the wrapper is still in use.
//
// Strategy: attach a cleanup to a synthetic *op.Client (no network), wrap it,
// drop the only other reference, force GC, and assert the cleanup did NOT run.
// If Client.opClient ever stops anchoring the SDK client, the cleanup fires
// and the test fails.
func TestClientRetainsSDKClient(t *testing.T) {
	t.Parallel()

	wrapper, released := newClientForGCTest()

	runtime.GC()
	runtime.Gosched()

	select {
	case <-released:
		t.Fatal("op.Client was reclaimed by GC while wrapper is still reachable; " +
			"Client.opClient must hold a strong reference to prevent premature SDK finalizer execution")
	case <-time.After(2 * time.Second):
	}

	// Ensures the compiler does not consider wrapper dead before the GC pass
	// above, which would make the assertion vacuous.
	runtime.KeepAlive(wrapper)
}

// newClientForGCTest constructs a Client around a synthetic *op.Client (no
// network I/O) and returns a channel that is closed if the SDK client is
// garbage-collected. Encapsulating construction here lets the local sdkClient
// pointer go out of scope naturally — no explicit nil assignment required.
func newClientForGCTest() (*Client, chan struct{}) {
	sdkClient := &op.Client{SecretsAPI: &mockSecretsAPI{}}
	released := make(chan struct{})
	// The cleanup arg (the channel) must not equal the tracked pointer; using
	// a chan satisfies that constraint and is goroutine-safe for close().
	runtime.AddCleanup(sdkClient, func(ch chan struct{}) { close(ch) }, released)
	return &Client{opClient: sdkClient}, released
}
