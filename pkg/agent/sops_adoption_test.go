package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// referenceAgentConfigIdentity is where the reference Agent config points
// sops.age_identity_path.
const referenceAgentConfigIdentity = "/etc/picolet/secrets/age-identity"

// referenceAgentFleet returns a Fleet whose rootful test-host runs the
// example Fleet's picolet-system bundle config template; hostYML is
// test-host's host.yml.
func referenceAgentFleet(t *testing.T, hostYML string) map[string]string {
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "testdata", "example-fleet",
		"services", "picolet-system", "secrets", "picolet_system_config.yml.tmpl"))
	require.NoError(t, err)
	files := agentBundleFiles(agentBundleUnit(""), "picolet_system_config.yml.tmpl", string(tmpl))
	files["hosts/test-host/host.yml"] = hostYML
	return files
}

// TestAgentRefusesSopsAdoptionWithoutDeliveredIdentity runs the Agent loop
// on the reference Agent config: the first commit (no age_recipients:)
// applies; a commit that adds age_recipients: makes the rendered config
// point sops.age_identity_path at an identity this Host was never given, so
// the self-restart guard fails that reconciliation, nothing is written, and
// the Agent keeps running on its old config.
func TestAgentRefusesSopsAdoptionWithoutDeliveredIdentity(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(referenceAgentConfigIdentity); err == nil {
		t.Skipf("%s exists on this machine", referenceAgentConfigIdentity)
	}
	plainHost := "hostname: test-host\nrole: server\nfeatures: []\n"
	bareDir, firstSHA := initFleetRepo(t, referenceAgentFleet(t, plainHost))
	statePath := filepath.Join(t.TempDir(), "state.json")
	sys, pod, fw := newBareMocks(t)
	setupApplyMocks(sys, pod, fw)
	sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	configs := recordSecretCreates(pod, "picolet_system_config")

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 100 * time.Millisecond,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg, WithSystemd(sys), WithPodman(pod), WithFileWriter(fw),
		WithRepoPath(filepath.Join(t.TempDir(), "clone")), WithStatePath(statePath))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	require.Eventually(t, func() bool { _, ok := eventMessage(a.statusStore.Snapshot(), "success", firstSHA); return ok },
		8*time.Second, 20*time.Millisecond, "the first commit applies")

	secondSHA := pushToTestRepo(t, bareDir, map[string]string{
		"hosts/test-host/host.yml": plainHost + "age_recipients:\n  - " + srv1Recipient + "\n",
	})
	require.Eventually(t, func() bool { _, ok := eventMessage(a.statusStore.Snapshot(), "failure", secondSHA); return ok },
		8*time.Second, 20*time.Millisecond, "the commit adding age_recipients: fails")

	select {
	case err := <-errCh:
		t.Fatalf("the Agent stopped after the refused reconciliation: %v", err)
	default:
	}
	cancel()
	require.NoError(t, <-errCh)

	msg, _ := eventMessage(a.statusStore.Snapshot(), "failure", secondSHA)
	assert.Contains(t, msg, "sops.age_identity_path")
	assert.Contains(t, msg, referenceAgentConfigIdentity)

	st, err := state.NewStore(statePath).Load()
	require.NoError(t, err)
	assert.Equal(t, firstSHA, st.AppliedSHA, "the Agent stays on the first commit")
	assert.Equal(t, secondSHA, st.FailedSHA)
	created := configs()
	require.Len(t, created, 1, "the refused config is never created")
	assert.NotContains(t, created[0], "sops:")
}

// recordSecretCreates lets pod create any secret and records the contents
// of the ones called name; the returned func reads them, in order.
func recordSecretCreates(pod *appliermocks.MockPodmanClient, name string) func() []string {
	var mu sync.Mutex
	var contents []string
	pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, n string, data []byte, _ bool) error {
			mu.Lock()
			defer mu.Unlock()
			if n == name {
				contents = append(contents, string(data))
			}
			return nil
		}).Maybe()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(contents)
	}
}

// eventMessage returns the message of the first reconcile event with result
// for sha, and whether there is one.
func eventMessage(snap status.Snapshot, result, sha string) (string, bool) {
	i := slices.IndexFunc(snap.Events, func(e status.ReconcileEvent) bool {
		return e.Result == result && e.SHA == sha
	})
	if i < 0 {
		return "", false
	}
	return snap.Events[i].Message, true
}
