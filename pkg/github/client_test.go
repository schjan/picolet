package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestPEM creates a temporary RSA private key PEM file for testing.
func writeTestPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "test-key.pem")
	data := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func TestNewClient(t *testing.T) {
	t.Parallel()

	pemPath := writeTestPEM(t)

	c, err := NewClient(12345, 67890, pemPath, "https://github.com/org/repo.git")
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewClientInvalidPEM(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a pem"), 0o600))

	_, err := NewClient(12345, 67890, path, "https://github.com/org/repo.git")
	require.Error(t, err)
}

func TestNewClientInvalidURL(t *testing.T) {
	t.Parallel()

	pemPath := writeTestPEM(t)

	_, err := NewClient(12345, 67890, pemPath, "https://gitlab.com/org/repo.git")
	require.Error(t, err)
	assert.ErrorContains(t, err, "not a GitHub URL")
}

func TestClientGitAuth(t *testing.T) {
	t.Parallel()

	// Fake GitHub API that returns an installation token.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/67890/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test_token_123","expires_at":"2099-01-01T00:00:00Z"}`))
	})

	srv := httptest.NewTestServer(t, mux)
	srv.Start()

	c, err := newClientWithBaseURL(12345, 67890, writeTestPEM(t), "https://github.com/org/repo.git", srv.URL)
	require.NoError(t, err)

	auth, err := c.GitAuth(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, auth)
}
