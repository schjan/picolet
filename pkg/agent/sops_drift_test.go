package agent

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/internal/testsops"
)

// TEST-ONLY public keys of the picolet fixture corpus (testdata/sops).
const (
	srv1Recipient = "age1eed9cxu2y3xx2s3syz95sanhy3xuajtpmjsnz63skgwl9yvw0g5su77pfq"
	srv3Recipient = "age1pre96p2krmjun420xp2jtzgzyryr3gvvvzqvqj439nzvcrjx953sj5vmaa"
)

const identityDriftWarning = "the Agent's age identities match none of its Host's age_recipients"

// driftHostYML is test-host's host.yml declaring recipients.
func driftHostYML(recipients ...string) string {
	y := "hostname: test-host\nrole: server\nfeatures: []\nage_recipients:\n"
	for _, r := range recipients {
		y += "  - " + r + "\n"
	}
	return y
}

// TestAgentWarnsOnIdentityDrift runs the Agent loop with identity srv-1 on a
// Fleet whose file is encrypted to srv-1: when test-host's age_recipients:
// name another key the decrypt still succeeds and the Agent warns on every
// reconciliation that decrypts; a declaration that holds srv-1 (alone or
// next to a rotation key) does not warn, nor does a reconciliation that
// decrypts nothing.
func TestAgentWarnsOnIdentityDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		recipients []string
		secret     string
		wantWarn   bool
	}{
		{name: "drifted", recipients: []string{srv3Recipient}, secret: dbPasswordSecret, wantWarn: true},
		{name: "matching", recipients: []string{srv1Recipient}, secret: dbPasswordSecret},
		{name: "rotation", recipients: []string{srv3Recipient, strings.ToUpper(srv1Recipient)}, secret: dbPasswordSecret},
		{name: "no sops refs", recipients: []string{srv3Recipient}, secret: "plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := sopsFleet(tc.secret, map[string]string{
				"secrets/db.sops.yaml":     testsops.CorpusText(t, "values.sops.yaml"),
				"hosts/test-host/host.yml": driftHostYML(tc.recipients...),
			})
			h := newSopsHarness(t, files, writeIdentityFile(t, "srv-1"))

			require.NoError(t, h.tick(t))
			// A second reconciliation of the same commit changes nothing.
			st, err := h.store.Load()
			require.NoError(t, err)
			res, err := h.a.ReconcileOnce(t.Context(), st.AppliedSHA, st, h.store)
			require.NoError(t, err)
			assert.False(t, res.HasChanges)
			if tc.secret == dbPasswordSecret {
				assert.Equal(t, "hunter2-test", h.secret("db_password"), "the decrypt succeeds")
			}

			warnings := driftWarnings(h.logs)
			if !tc.wantWarn {
				assert.Empty(t, warnings)
				return
			}
			require.Len(t, warnings, 2, "one warning per reconciliation")
			w := warnings[0]
			assert.Equal(t, "test-host", w.Attrs["host"])
			assert.Equal(t, "["+srv3Recipient+"]", w.Attrs["age_recipients"])
			assert.Equal(t, "["+srv1Recipient+"]", w.Attrs["identity_recipients"])
			assert.Zero(t, h.logs.Count(slog.LevelError), h.logs.String())
		})
	}
}

func driftWarnings(logs *testlog.Recorder) []testlog.Record {
	var out []testlog.Record
	for _, r := range logs.Records() {
		if r.Level == slog.LevelWarn && r.Message == identityDriftWarning {
			out = append(out, r)
		}
	}
	return out
}
