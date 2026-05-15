package protonpass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner is a hand-written cmdRunner for unit tests. It dispatches based
// on the args[0] verb (vault/login/item) so tests can configure each phase
// independently.
type fakeRunner struct {
	probeErr   error
	loginErr   error
	loginStder []byte
	resolveFn  func(ctx context.Context, env []string, ref string) (stdout []byte, stderr []byte, err error)
	calls      atomic.Int32
	loginCalls atomic.Int32
}

func (f *fakeRunner) Run(ctx context.Context, env []string, _ string, args ...string) ([]byte, []byte, error) {
	f.calls.Add(1)
	if len(args) == 0 {
		return nil, nil, errors.New("no args")
	}
	switch args[0] {
	case "vault":
		return nil, nil, f.probeErr
	case "login":
		f.loginCalls.Add(1)
		return nil, f.loginStder, f.loginErr
	case "item":
		if len(args) < 3 {
			return nil, nil, errors.New("item: missing ref")
		}
		if f.resolveFn == nil {
			return nil, nil, errors.New("resolveFn not set")
		}
		return f.resolveFn(ctx, env, args[2])
	}
	return nil, nil, errors.New("unknown verb: " + args[0])
}

func writeTempFileContent(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// newTestClient builds a Client in Lazy mode (no PAT) and swaps in the runner.
func newTestClient(t *testing.T, runner cmdRunner) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test"})
	require.NoError(t, err)
	c.runner = runner
	return c
}

func TestEnsureSessionAlreadyValid(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{} // probeErr nil → session valid
	c := newTestClient(t, r)

	require.NoError(t, c.EnsureSession(t.Context()))
	require.NoError(t, c.EnsureSession(t.Context())) // idempotent

	// vault list called once (cached after success).
	assert.Equal(t, int32(1), r.calls.Load())
	assert.Equal(t, int32(0), r.loginCalls.Load())
}

func TestEnsureSessionLoginRequiredButNoPAT(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{probeErr: errors.New("not logged in")}
	c := newTestClient(t, r)

	err := c.EnsureSession(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no active session and no PAT configured")
	assert.Equal(t, int32(0), r.loginCalls.Load())
}

func TestEnsureSessionAutoLoginWithPATAndKey(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")
	keyPath := writeTempFileContent(t, "key", "0123456789abcdef")

	r := &fakeRunner{probeErr: errors.New("not logged in")}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, EncryptionKeyPath: keyPath})
	require.NoError(t, err)
	c.runner = r

	require.NoError(t, c.EnsureSession(t.Context()))
	assert.Equal(t, int32(1), r.loginCalls.Load())

	// Idempotent — no second login.
	require.NoError(t, c.EnsureSession(t.Context()))
	assert.Equal(t, int32(1), r.loginCalls.Load())
}

func TestEnsureSessionLoginFailsSurfacedSanitized(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")
	keyPath := writeTempFileContent(t, "key", "0123456789abcdef")

	r := &fakeRunner{
		probeErr:   errors.New("not logged in"),
		loginErr:   errors.New("exit status 1"),
		loginStder: []byte("invalid token: pst_abcdefghijklmnopqrstuvwxyz0123456789=="),
	}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, EncryptionKeyPath: keyPath})
	require.NoError(t, err)
	c.runner = r

	err = c.EnsureSession(t.Context())
	require.Error(t, err)
	// PAT must NOT appear in the error; sanitizer redacts long token-shaped runs.
	assert.NotContains(t, err.Error(), "pst_abcdefghijklmnopqrstuvwxyz")
	assert.Contains(t, err.Error(), "<redacted>")
}

func TestResolveInvalidRef(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &fakeRunner{})

	_, err := c.Resolve(t.Context(), "not-a-pass-ref")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid pass:// reference")
}

func TestResolveHappyPath(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{
		resolveFn: func(_ context.Context, _ []string, ref string) ([]byte, []byte, error) {
			assert.Equal(t, "pass://share/item/password", ref)
			return []byte(`"s3cret"`), nil, nil
		},
	}
	c := newTestClient(t, r)

	val, err := c.Resolve(t.Context(), "pass://share/item/password")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", val)
}

func TestResolveAcceptsObjectShape(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{
		resolveFn: func(_ context.Context, _ []string, _ string) ([]byte, []byte, error) {
			return []byte(`{"password": "from-object"}`), nil, nil
		},
	}
	c := newTestClient(t, r)

	val, err := c.Resolve(t.Context(), "pass://share/item/password")
	require.NoError(t, err)
	assert.Equal(t, "from-object", val)
}

func TestResolveAllPartialFailure(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{
		resolveFn: func(_ context.Context, _ []string, ref string) ([]byte, []byte, error) {
			if ref == "pass://share/bad/field" {
				return nil, []byte("not found"), errors.New("exit status 1")
			}
			return []byte(`"value-of-` + ref + `"`), nil, nil
		},
	}
	c := newTestClient(t, r)

	results, err := c.ResolveAll(t.Context(), []string{
		"pass://share/good/field",
		"pass://share/bad/field",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pass://share/bad/field")
	assert.Equal(t, "value-of-pass://share/good/field", results["pass://share/good/field"])
	_, hasBad := results["pass://share/bad/field"]
	assert.False(t, hasBad)
}

func TestResolveAllEmpty(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &fakeRunner{})

	results, err := c.ResolveAll(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestParseItemViewJSONErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not json", "raw text"},
		{"object with two fields", `{"a":"x","b":"y"}`},
		{"object with non-string value", `{"a":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseItemViewJSON([]byte(tc.in))
			assert.Error(t, err)
		})
	}
}

func TestNewClientPATWithoutKey(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test")
	_, err := NewClient(ClientConfig{PATPath: patPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encryption_key_path is required")
}

func TestNewClientEmptyEncryptionKey(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test")
	keyPath := writeTempFileContent(t, "key", "")
	_, err := NewClient(ClientConfig{PATPath: patPath, EncryptionKeyPath: keyPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encryption key file is empty")
}
