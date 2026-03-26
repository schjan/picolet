package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupDeploymentTest(t *testing.T, handler http.Handler) *DeploymentReporter {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := newClientWithBaseURL(12345, 67890, writeTestPEM(t), "https://github.com/org/repo.git", srv.URL)
	require.NoError(t, err)

	return NewDeploymentReporter(c, "test-host")
}

func TestCreateDeployment(t *testing.T) {
	t.Parallel()

	var gotDeployReq map[string]any
	mux := http.NewServeMux()

	mux.HandleFunc("POST /app/installations/67890/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z"}`))
	})

	mux.HandleFunc("POST /api/v3/repos/org/repo/deployments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotDeployReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42}`))
	})

	mux.HandleFunc("POST /api/v3/repos/org/repo/deployments/42/statuses", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})

	reporter := setupDeploymentTest(t, mux)
	id, err := reporter.CreateDeployment(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Equal(t, int64(42), id)
	assert.Equal(t, "abc123", gotDeployReq["ref"])
	assert.Equal(t, "test-host", gotDeployReq["environment"])
}

func TestReportSuccess(t *testing.T) {
	t.Parallel()

	var gotStatus map[string]any
	mux := http.NewServeMux()

	mux.HandleFunc("POST /app/installations/67890/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z"}`))
	})

	mux.HandleFunc("POST /api/v3/repos/org/repo/deployments/42/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotStatus)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":2}`))
	})

	reporter := setupDeploymentTest(t, mux)
	err := reporter.ReportSuccess(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, "success", gotStatus["state"])
}

func TestReportFailureTruncatesDescription(t *testing.T) {
	t.Parallel()

	var gotStatus map[string]any
	mux := http.NewServeMux()

	mux.HandleFunc("POST /app/installations/67890/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z"}`))
	})

	mux.HandleFunc("POST /api/v3/repos/org/repo/deployments/42/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotStatus)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":3}`))
	})

	reporter := setupDeploymentTest(t, mux)
	longErr := errors.New(string(make([]byte, 200)))
	err := reporter.ReportFailure(context.Background(), 42, longErr)
	require.NoError(t, err)
	assert.Equal(t, "failure", gotStatus["state"])

	desc, ok := gotStatus["description"].(string)
	require.True(t, ok, "description should be a string")
	assert.LessOrEqual(t, len(desc), 140)
}
