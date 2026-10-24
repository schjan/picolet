package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	agentmocks "github.com/schjan/picolet/mocks/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/mqtt"
	"github.com/schjan/picolet/pkg/state"
)

// writeSecretOnlyFleet writes a Fleet whose only Managed File is one bare
// direct secret with the value "hunter2".
func writeSecretOnlyFleet(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "fleet.yml", `images: {}
ports: {picolet_system_metrics: 9418}
`)
	writeTestFile(t, repoDir, "assignments.yml", `base:
  services:
    - app
`)
	writeTestFile(t, repoDir, "hosts/test-host/host.yml", `hostname: test-host
role: server
features: []
`)
	writeTestFile(t, repoDir, "services/app/secrets/app_password", "hunter2")
	return repoDir
}

// TestSecretContentHashNotExposed audits the Agent's surfaces besides the
// dashboard view (covered in pkg/dashboard): /metrics, /health and the MQTT
// status topics. A bare direct secret's content hash is an unsalted SHA-256
// of the secret value, so none of them may carry it (#250). /webhook only
// accepts a signed trigger and reads no state.
func TestSecretContentHashNotExposed(t *testing.T) {
	t.Parallel()
	repoDir := writeSecretOnlyFleet(t)

	sys, pod, fw := newBareMocks(t)
	pod.EXPECT().SecretCreate(mock.Anything, "app_password", []byte("hunter2"), false).Return(nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()

	var published []mqtt.Status
	mqttMock := agentmocks.NewMockMQTTClient(t)
	mqttMock.EXPECT().PublishStatus(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, s mqtt.Status) error {
			published = append(published, s)
			return nil
		})

	cfg := &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir()}
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(repoDir), WithMQTT(mqttMock))

	st := state.NewState()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, store.Save(st))
	_, err := a.ReconcileOnce(t.Context(), "head-sha", st, store)
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)
	var secretHash string
	for _, mf := range loaded.ManagedFiles {
		if mf.Category == config.CategorySecret {
			secretHash = strings.TrimPrefix(mf.Hash, "sha256:")
		}
	}
	require.GreaterOrEqual(t, len(secretHash), 8, "premise: the state records the secret's content hash")
	short := secretHash[:8]

	a.publishMQTTStatus(t.Context(), loaded, time.Now())
	require.Len(t, published, 1)
	assert.NotContains(t, fmt.Sprintf("%+v", published[0]), short, "MQTT status")

	a.ready.Store(true)
	mux := a.newMux()
	for _, path := range []string{"/metrics", "/health"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body, err := io.ReadAll(rec.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.NotContains(t, string(body), short, path)
	}
}
