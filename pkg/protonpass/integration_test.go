//go:build protonpass_integration

package protonpass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Shared client logged in once for all integration tests in this package.
// Each test would otherwise build its own Client and call `pass-cli login`
// in parallel, hammering Proton's auth endpoint and risking per-PAT
// rate limiting / server-side serialization.
var (
	sharedClient    *Client
	sharedClientErr error
)

// TestMain logs in once and shares the resulting Client with every test.
// The shared SessionDir lives in os.MkdirTemp (not t.TempDir, which has no
// suitable owning test) and is removed at process exit.
func TestMain(m *testing.M) {
	cleanup := setupSharedClient()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func setupSharedClient() func() {
	noop := func() {}
	pat := os.Getenv("PP_PERSONAL_ACCESS_TOKEN")
	if pat == "" {
		sharedClientErr = errors.New("PP_PERSONAL_ACCESS_TOKEN not set")
		return noop
	}
	dir, err := os.MkdirTemp("", "pp-integration-")
	if err != nil {
		sharedClientErr = fmt.Errorf("mkdir temp: %w", err)
		return noop
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	patPath := filepath.Join(dir, "pat")
	// gosec G703: dir comes from os.MkdirTemp, fully controlled by this test
	// package; no caller input flows into the path.
	if err := os.WriteFile(patPath, []byte(pat), 0o600); err != nil { //nolint:gosec
		sharedClientErr = fmt.Errorf("write PAT: %w", err)
		return cleanup
	}
	c, err := NewClient(ClientConfig{
		PATPath:    patPath,
		SessionDir: filepath.Join(dir, "session"),
	})
	if err != nil {
		sharedClientErr = fmt.Errorf("new client: %w", err)
		return cleanup
	}
	if err := c.EnsureSession(context.Background()); err != nil {
		sharedClientErr = fmt.Errorf("ensure session: %w", err)
		return cleanup
	}
	sharedClient = c
	return cleanup
}

// integrationClient returns the shared, pre-logged-in Client, or skips/fails
// the calling test depending on why setup couldn't complete.
func integrationClient(t *testing.T) *Client {
	t.Helper()
	if sharedClient != nil {
		return sharedClient
	}
	if os.Getenv("PP_PERSONAL_ACCESS_TOKEN") == "" {
		t.Skip("PP_PERSONAL_ACCESS_TOKEN not set")
	}
	require.NoError(t, sharedClientErr)
	return sharedClient
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

func TestEnsureSessionIntegration(t *testing.T) {
	t.Parallel()
	c := integrationClient(t)
	require.NoError(t, c.EnsureSession(t.Context()))
}

func TestResolveIntegration(t *testing.T) {
	t.Parallel()
	c := integrationClient(t)
	ref := requireEnv(t, "PP_TEST_REF")

	val, err := c.Resolve(t.Context(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, val)
}

func TestResolveAllIntegration(t *testing.T) {
	t.Parallel()
	c := integrationClient(t)
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
	c := integrationClient(t)
	ref := requireEnv(t, "PP_TEST_REF")

	badRef := "pass://nonexistent-share/nonexistent-item/field"
	results, err := c.ResolveAll(t.Context(), []string{ref, badRef})
	require.Error(t, err)
	require.Contains(t, results, ref)
	require.NotEmpty(t, results[ref])
	require.NotContains(t, results, badRef)
}
