package protonpass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner is a hand-written cmdRunner for unit tests. It dispatches based
// on the args[0] verb (test/login/item) so tests can configure each phase
// independently.
type fakeRunner struct {
	probeErr   error
	probeErrs  []error
	probeStder []byte
	loginErr   error
	loginStder []byte
	resolveFn  func(ctx context.Context, env []string, ref string) (stdout []byte, stderr []byte, err error)
	calls      atomic.Int32
	probeCalls atomic.Int32
	loginCalls atomic.Int32
}

func (f *fakeRunner) Run(ctx context.Context, env []string, _ string, args ...string) ([]byte, []byte, error) {
	f.calls.Add(1)
	if len(args) == 0 {
		return nil, nil, errors.New("no args")
	}
	switch args[0] {
	case "test":
		idx := int(f.probeCalls.Add(1)) - 1
		if len(f.probeErrs) > 0 {
			if idx < len(f.probeErrs) {
				return nil, f.probeStder, f.probeErrs[idx]
			}
			return nil, f.probeStder, f.probeErrs[len(f.probeErrs)-1]
		}
		return nil, f.probeStder, f.probeErr
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

	// Session checks are intentionally not cached; expired sessions should be
	// detected before later secret-resolution batches.
	assert.Equal(t, int32(2), r.calls.Load())
	assert.Equal(t, int32(2), r.probeCalls.Load())
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

func TestEnsureSessionProbeErrorSanitized(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{
		probeErr:   errors.New("exit status 1"),
		probeStder: []byte("invalid session token abcdefghijklmnopqrstuvwxyz0123456789"),
	}
	c := newTestClient(t, r)

	err := c.EnsureSession(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<redacted>")
	assert.NotContains(t, err.Error(), "abcdefghijklmnopqrstuvwxyz")
}

func TestEnsureSessionAutoLoginWithPAT(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")

	r := &fakeRunner{probeErrs: []error{errors.New("not logged in"), nil, nil}}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, SessionDir: t.TempDir()})
	require.NoError(t, err)
	c.runner = r

	require.NoError(t, c.EnsureSession(t.Context()))
	assert.Equal(t, int32(1), r.loginCalls.Load())
	assert.Equal(t, int32(2), r.probeCalls.Load())

	// Later checks still run, but a valid session does not log in again.
	require.NoError(t, c.EnsureSession(t.Context()))
	assert.Equal(t, int32(1), r.loginCalls.Load())
	assert.Equal(t, int32(3), r.probeCalls.Load())
}

func TestEnsureSessionAutoLoginRequiresPostLoginSession(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")

	r := &fakeRunner{probeErrs: []error{errors.New("not logged in"), errors.New("still not logged in")}}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, SessionDir: t.TempDir()})
	require.NoError(t, err)
	c.runner = r

	err = c.EnsureSession(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login completed but session check failed")
	assert.Equal(t, int32(1), r.loginCalls.Load())
	assert.Equal(t, int32(2), r.probeCalls.Load())
}

func TestEnsureSessionLoginFailsSurfacedSanitized(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")

	r := &fakeRunner{
		probeErr:   errors.New("not logged in"),
		loginErr:   errors.New("exit status 1"),
		loginStder: []byte("invalid token: pst_abcdefghijklmnopqrstuvwxyz0123456789=="),
	}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, SessionDir: t.TempDir()})
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
	assert.Contains(t, err.Error(), "missing pass:// prefix")
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

// Since ResolveAll lacks an upfront IsRef check, the new ParseRef inside
// resolveLocked is the validation point for batch callers. Anchor that
// contract here so a future refactor cannot silently let invalid refs reach
// the pass-cli subprocess.
func TestResolveAllRejectsInvalidRef(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{
		resolveFn: func(_ context.Context, _ []string, ref string) ([]byte, []byte, error) {
			return []byte(`"value-of-` + ref + `"`), nil, nil
		},
	}
	c := newTestClient(t, r)

	results, err := c.ResolveAll(t.Context(), []string{
		"pass://share/good/field",
		"not-a-pass-ref",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing pass:// prefix")
	assert.Equal(t, "value-of-pass://share/good/field", results["pass://share/good/field"])
	_, hasBad := results["not-a-pass-ref"]
	assert.False(t, hasBad)
}

func TestParseItemViewJSONHappyPaths(t *testing.T) {
	t.Parallel()
	ref := PassRef{Share: "s", Item: "i", Field: "section/password"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw string", `"raw-value"`, "raw-value"},
		{"single-field object", `{"password":"single-value"}`, "single-value"},
		{"multi-field object matching tail segment", `{"password":"matched","modified_at":"2026-05-18"}`, "matched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseItemViewJSON([]byte(tc.in), ref)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseItemViewJSONErrors(t *testing.T) {
	t.Parallel()
	ref := PassRef{Share: "s", Item: "i", Field: "password"}
	cases := []struct {
		name    string
		in      string
		wantMsg string
	}{
		{"empty", "", "empty"},
		{"not json", "raw text", "decoding"},
		{"empty object", `{}`, "field \"password\" not present"},
		{"multi-field object missing target", `{"login":"x","note":"y"}`, "field \"password\" not present"},
		{"multi-field object non-string target", `{"password":42,"note":"y"}`, "value is float64"},
		{"single-field object non-string value", `{"password":42}`, "field value is float64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseItemViewJSON([]byte(tc.in), ref)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestBuildBaseEnvAllowlistsHostEnvAndStripsProtonSecrets(t *testing.T) {
	t.Setenv("HOME", "/tmp/picolet-home")
	t.Setenv("HTTPS_PROXY", "http://proxy.example")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("PROTON_PASS_PERSONAL_ACCESS_TOKEN", "from-env")
	t.Setenv("PROTON_PASS_ENCRYPTION_KEY", "from-env")
	t.Setenv("PROTON_PASS_KEY_PROVIDER", "from-env")
	t.Setenv("PROTON_PASS_SESSION_DIR", "from-env")
	t.Setenv("PROTON_PASS_NO_UPDATE_CHECK", "0")

	env, err := buildBaseEnv(ClientConfig{})
	require.NoError(t, err)

	assertEnvContains(t, env, "HOME", "/tmp/picolet-home")
	assertEnvContains(t, env, "HTTPS_PROXY", "http://proxy.example")
	assertEnvContains(t, env, "PROTON_PASS_NO_UPDATE_CHECK", "1")
	assertEnvMissing(t, env, "GITHUB_TOKEN")
	assertEnvMissing(t, env, "PROTON_PASS_PERSONAL_ACCESS_TOKEN")
	assertEnvMissing(t, env, "PROTON_PASS_ENCRYPTION_KEY")
	assertEnvMissing(t, env, "PROTON_PASS_KEY_PROVIDER")
	assertEnvMissing(t, env, "PROTON_PASS_SESSION_DIR")
}

func TestBuildBaseEnvPATModeOverlaysSessionAndFsProvider(t *testing.T) {
	patPath := writeTempFileContent(t, "pat", "pst_test")
	sessionDir := filepath.Join(t.TempDir(), "session")

	env, err := buildBaseEnv(ClientConfig{PATPath: patPath, SessionDir: sessionDir})
	require.NoError(t, err)

	assertEnvContains(t, env, "PROTON_PASS_SESSION_DIR", sessionDir)
	assertEnvContains(t, env, "PROTON_PASS_KEY_PROVIDER", "fs")
	assertEnvMissing(t, env, "PROTON_PASS_ENCRYPTION_KEY")
	assert.NoDirExists(t, sessionDir)
}

func TestEnsureSessionCreatesSessionDirOnUse(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")
	sessionDir := filepath.Join(t.TempDir(), "session")
	r := &fakeRunner{probeErrs: []error{nil}}
	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath, SessionDir: sessionDir})
	require.NoError(t, err)
	c.runner = r

	require.NoError(t, c.EnsureSession(t.Context()))
	assert.DirExists(t, sessionDir)
	info, err := os.Stat(sessionDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestNewClientPATModeDefaultSessionDir(t *testing.T) {
	t.Parallel()
	patPath := writeTempFileContent(t, "pat", "pst_test_token::keymaterial")

	c, err := NewClient(ClientConfig{CLIPath: "pass-cli-test", PATPath: patPath})
	require.NoError(t, err)
	assert.Equal(t, DefaultSessionDir, c.sessionDir)
	assertEnvContains(t, c.env, "PROTON_PASS_SESSION_DIR", DefaultSessionDir)
}

func TestEffectiveSessionDir(t *testing.T) {
	t.Parallel()
	assert.Empty(t, effectiveSessionDir(ClientConfig{}))
	assert.Equal(t, DefaultSessionDir, effectiveSessionDir(ClientConfig{PATPath: "/pat"}))
	assert.Equal(t, "/custom", effectiveSessionDir(ClientConfig{PATPath: "/pat", SessionDir: "/custom"}))
	assert.Equal(t, "/custom", effectiveSessionDir(ClientConfig{SessionDir: "/custom"}))
}

func assertEnvContains(t *testing.T, env []string, key, want string) {
	t.Helper()
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			assert.Equal(t, key+"="+want, entry)
			return
		}
	}
	assert.Failf(t, "missing env", "missing %s", key)
}

func assertEnvMissing(t *testing.T, env []string, key string) {
	t.Helper()
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			assert.Failf(t, "unexpected env", "unexpected %s", entry)
			return
		}
	}
}
