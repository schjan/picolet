package githubauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/resolver"
)

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestNewClientFromConfigDirect(t *testing.T) {
	t.Parallel()

	pemPath := filepath.Join(t.TempDir(), "github-app.pem")
	require.NoError(t, os.WriteFile(pemPath, testPrivateKeyPEM(t), 0o600))

	cfg := &agentcfg.Config{
		RepoURL:              "https://github.com/org/repo.git",
		GitHubAppID:          12345,
		GitHubInstallationID: 67890,
		GitHubPrivateKeyPath: pemPath,
	}

	c, appID, err := NewClientFromConfig(context.Background(), cfg, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, int64(12345), appID)
	assert.Equal(t, "org", c.Owner)
	assert.Equal(t, "repo", c.Repo)
}

func TestNewClientFromConfigRefs(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{
		RepoURL: "https://github.com/org/repo.git",
		OnePassword: &agentcfg.OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	reader := resolver.OpSecretReader(func(_ context.Context, refs []string) (map[string]string, error) {
		assert.ElementsMatch(t, []string{
			"op://vault/app/id",
			"op://vault/app/installation",
			"op://vault/app/private_key",
		}, refs)
		return map[string]string{
			"op://vault/app/id":           "12345",
			"op://vault/app/installation": "67890",
			"op://vault/app/private_key":  string(testPrivateKeyPEM(t)),
		}, nil
	})

	c, appID, err := NewClientFromConfig(context.Background(), cfg, reader)
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, int64(12345), appID)
	assert.Equal(t, "org", c.Owner)
	assert.Equal(t, "repo", c.Repo)
}

func TestNewClientFromConfigRefsRequireOnePasswordReader(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{
		RepoURL: "https://github.com/org/repo.git",
		OnePassword: &agentcfg.OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	_, _, err := NewClientFromConfig(context.Background(), cfg, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "onepassword must be configured")
}

func TestNewClientFromConfigRefsMissingResult(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{
		RepoURL: "https://github.com/org/repo.git",
		OnePassword: &agentcfg.OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	reader := resolver.OpSecretReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{
			"op://vault/app/id":          "12345",
			"op://vault/app/private_key": string(testPrivateKeyPEM(t)),
		}, nil
	})

	_, _, err := NewClientFromConfig(context.Background(), cfg, reader)
	require.Error(t, err)
	assert.ErrorContains(t, err, "missing onepassword.github_installation_id_ref")
}

func TestNewClientFromConfigRefsInvalidAppID(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{
		RepoURL: "https://github.com/org/repo.git",
		OnePassword: &agentcfg.OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	reader := resolver.OpSecretReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{
			"op://vault/app/id":           "abc",
			"op://vault/app/installation": "67890",
			"op://vault/app/private_key":  string(testPrivateKeyPEM(t)),
		}, nil
	})

	_, _, err := NewClientFromConfig(context.Background(), cfg, reader)
	require.Error(t, err)
	assert.ErrorContains(t, err, "onepassword.github_app_id_ref is not a valid integer")
}

func TestNewClientFromConfigRefsEmptyPrivateKey(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{
		RepoURL: "https://github.com/org/repo.git",
		OnePassword: &agentcfg.OnePasswordConfig{
			GitHubAppIDRef:        "op://vault/app/id",
			GitHubInstallationRef: "op://vault/app/installation",
			GitHubPrivateKeyRef:   "op://vault/app/private_key",
		},
	}

	reader := resolver.OpSecretReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{
			"op://vault/app/id":           "12345",
			"op://vault/app/installation": "67890",
			"op://vault/app/private_key":  "\n\n",
		}, nil
	})

	_, _, err := NewClientFromConfig(context.Background(), cfg, reader)
	require.Error(t, err)
	assert.ErrorContains(t, err, "resolved to an empty value")
}

func TestNewClientFromConfigNilConfig(t *testing.T) {
	t.Parallel()

	_, _, err := NewClientFromConfig(context.Background(), nil, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "agent config is required")
}
