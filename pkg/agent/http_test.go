package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
)

func TestStartHTTPBindFailure(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t, &agentcfg.Config{ListenAddr: "127.0.0.1:70000"})
	shutdown, err := a.startHTTP()
	require.Error(t, err)
	assert.Nil(t, shutdown)
}

func TestTriggerReconcileChannel(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := newTestAgent(t, cfg)

	// First send should succeed
	a.triggerReconcile()

	// Second send should be dropped (channel full, buffered=1)
	a.triggerReconcile()

	// Drain: should get exactly one signal
	select {
	case <-a.webhookCh:
		// expected
	default:
		t.Fatal("expected signal in webhookCh")
	}

	// Channel should now be empty
	select {
	case <-a.webhookCh:
		t.Fatal("channel should be empty after drain")
	default:
		// expected
	}
}

func TestWebhookOnHTTPServer(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := newTestAgent(t, cfg)

	srv := httptest.NewTestServer(t, a.newMux())
	client := srv.Client()

	resp, err := client.Post(srv.URL+"/webhook", "application/json", strings.NewReader("{}")) //nolint:noctx // test helper, no context needed
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Channel should have received a signal
	select {
	case <-a.webhookCh:
		// expected
	default:
		t.Fatal("expected signal in webhookCh after POST /webhook")
	}
}

func TestHealthEndpoint_ReturnsUnavailableBeforeFirstTick(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := newTestAgent(t, cfg)

	srv := httptest.NewTestServer(t, a.newMux())
	client := srv.Client()

	resp, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestHealthEndpoint_ReturnsOKAfterSuccessfulTick(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = time.Hour
	srv := httptest.NewTestServer(t, h.a.newMux())
	client := srv.Client()

	// Wait until agent becomes ready
	h.runUntil(t, func() bool {
		resp, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "/health should return 200 after first tick")
}

//nolint:funlen // integration test: setup + two sub-tests for valid/invalid webhook
func TestWebhookTriggersReconciliation(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, networkFleet())

	metrics.Register(nil)

	secretFile := filepath.Join(t.TempDir(), "webhook-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("test-secret"), 0o600))

	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = time.Hour // only webhook-triggered reconciliation
	h.cfg.WebhookSecretPath = secretFile

	ctx := t.Context()
	stop := h.start(t)
	defer stop()

	srv := httptest.NewTestServer(t, h.a.newMux())
	client := srv.Client()

	// Wait for initial tick to complete
	require.Eventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.written) > 0
	}, 10*time.Second, 50*time.Millisecond, "initial tick should write files")

	// Push a new commit that adds hello.container
	pushToTestRepo(t, bareDir, map[string]string{
		"assignments.yml": `base:
  paths:
    - quadlets/networks/internal.network
    - quadlets/containers/hello.container
`,
		"quadlets/containers/hello.container": `[Container]
Image=hello-world:latest
`,
	})

	// Wrong signature → 403, no trigger
	badReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook", strings.NewReader("{}"))
	require.NoError(t, err)
	badReq.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	badResp, err := client.Do(badReq)
	require.NoError(t, err)
	defer badResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, badResp.StatusCode)

	select {
	case <-h.a.webhookCh:
		t.Fatal("trigger should not have been called")
	default:
		// expected — handler returned 403, no trigger
	}

	// Valid signature → 202, triggers reconciliation
	body := []byte("{}")
	sig := ComputeSignature(body, "test-secret")
	goodReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	require.NoError(t, err)
	goodReq.Header.Set("X-Hub-Signature-256", sig)
	goodResp, err := client.Do(goodReq)
	require.NoError(t, err)
	defer goodResp.Body.Close()
	assert.Equal(t, http.StatusAccepted, goodResp.StatusCode)

	require.Eventually(t, func() bool {
		_, ok := h.file("/etc/containers/systemd/picolet/hello.container")
		return ok
	}, 10*time.Second, 50*time.Millisecond, "webhook should trigger reconciliation that writes hello.container")
}

func TestHealthEndpoint_Returns503AfterConsecutiveDBusFailures(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := newTestAgent(t, cfg)
	a.ready.Store(true)

	srv := httptest.NewTestServer(t, a.newMux())
	client := srv.Client()

	// Below threshold: should return 200
	a.consecutiveHealthFailures.Store(2)
	resp, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// At threshold: should return 503
	a.consecutiveHealthFailures.Store(3)
	resp2, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)

	// Recovery: counter resets below threshold
	a.consecutiveHealthFailures.Store(0)
	resp3, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
}

func TestHealthEndpoint_Returns200WhenPausedEvenWithDBusDown(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{
		Hostname: "test",
		RepoURL:  "https://example.com/repo.git",
	}
	a := newTestAgent(t, cfg)
	a.ready.Store(true)
	a.paused.Store(true)
	a.consecutiveHealthFailures.Store(5) // well above threshold

	srv := httptest.NewTestServer(t, a.newMux())
	client := srv.Client()

	resp, err := client.Get(srv.URL + "/health") //nolint:noctx // test helper
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUpdateHealthFailures(t *testing.T) {
	t.Parallel()

	t.Run("all errors increments counter", func(t *testing.T) {
		t.Parallel()
		a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

		a.updateHealthFailures(&health.CheckResult{
			Errors: []error{fmt.Errorf("dbus dead"), fmt.Errorf("dbus dead")},
		})
		assert.Equal(t, int32(1), a.consecutiveHealthFailures.Load())
	})

	t.Run("mixed results resets counter", func(t *testing.T) {
		t.Parallel()
		a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})
		a.consecutiveHealthFailures.Store(5)

		a.updateHealthFailures(&health.CheckResult{
			Healthy: []string{"foo.service"},
			Errors:  []error{fmt.Errorf("dbus dead")},
		})
		assert.Equal(t, int32(0), a.consecutiveHealthFailures.Load())
	})

	t.Run("zero managed units stays at zero", func(t *testing.T) {
		t.Parallel()
		a := newTestAgent(t, &agentcfg.Config{Hostname: "test", RepoURL: "https://example.com/repo.git"})

		a.updateHealthFailures(&health.CheckResult{})
		assert.Equal(t, int32(0), a.consecutiveHealthFailures.Load())
	})
}
