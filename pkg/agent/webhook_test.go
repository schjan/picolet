package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookValidPostNoSecret(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, "")

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, int32(1), called.Load())
}

func TestWebhookValidPostWithHMAC(t *testing.T) {
	t.Parallel()
	secret := "test-secret"
	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretPath, []byte(secret), 0o600))

	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, secretPath)

	body := []byte("{}")
	sig := ComputeSignature(body, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, int32(1), called.Load())
}

func TestWebhookWrongHMAC(t *testing.T) {
	t.Parallel()
	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("correct-secret"), 0o600))

	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, secretPath)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	req.Header.Set("X-Hub-Signature-256", "sha256=wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, int32(0), called.Load())
}

func TestWebhookMissingSignatureWhenSecretConfigured(t *testing.T) {
	t.Parallel()
	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("my-secret"), 0o600))

	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, secretPath)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, int32(0), called.Load())
}

func TestWebhookGetNotAllowed(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, "")

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, int32(0), called.Load())
}

func TestWebhookSecretFileMissing(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	h := webhookHandler(func() { called.Add(1) }, "/nonexistent/secret")

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, int32(0), called.Load())
}

func TestComputeSignature(t *testing.T) {
	t.Parallel()
	sig := ComputeSignature([]byte("hello"), "secret")
	assert.True(t, strings.HasPrefix(sig, "sha256="))
	assert.Len(t, sig, 7+64) // "sha256=" + 64 hex chars

	// Same input should produce same output
	sig2 := ComputeSignature([]byte("hello"), "secret")
	assert.Equal(t, sig, sig2)

	// Different secret should produce different output
	sig3 := ComputeSignature([]byte("hello"), "other")
	assert.NotEqual(t, sig, sig3)
}
