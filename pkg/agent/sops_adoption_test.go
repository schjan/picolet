package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = 100 * time.Millisecond

	stop := h.start(t)
	require.Eventually(t, func() bool { _, ok := eventMessage(h.a.statusStore.Snapshot(), "success", firstSHA); return ok },
		8*time.Second, 20*time.Millisecond, "the first commit applies")

	secondSHA := pushToTestRepo(t, bareDir, map[string]string{
		"hosts/test-host/host.yml": plainHost + "age_recipients:\n  - " + srv1Recipient + "\n",
	})
	require.Eventually(t, func() bool { _, ok := eventMessage(h.a.statusStore.Snapshot(), "failure", secondSHA); return ok },
		8*time.Second, 20*time.Millisecond, "the commit adding age_recipients: fails")
	// stop fails the test when the Agent stopped after the refused
	// reconciliation.
	stop()

	msg, _ := eventMessage(h.a.statusStore.Snapshot(), "failure", secondSHA)
	assert.Contains(t, msg, "sops.age_identity_path")
	assert.Contains(t, msg, referenceAgentConfigIdentity)

	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, firstSHA, st.AppliedSHA, "the Agent stays on the first commit")
	assert.Equal(t, secondSHA, st.FailedSHA)
	require.Equal(t, 1, h.creates("picolet_system_config"), "the refused config is never created")
	assert.NotContains(t, h.secret("picolet_system_config"), "sops:")
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
