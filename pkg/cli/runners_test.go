package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
)

// `picolet apply` has no pending-restart bookkeeping: a managed unit that
// fails to restart makes the apply incomplete, so runApply saves no state and
// the next apply retries (a rebuilt image's consumer would otherwise never
// be restarted).
func TestApplyWithRollbackIncompleteOnFailedManagedRestart(t *testing.T) {
	t.Parallel()
	unit := filepath.Join(t.TempDir(), "app.container")
	require.NoError(t, os.WriteFile(unit, []byte("[Container]\nImage=old\n"), 0o600))
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "app.service").Return(assert.AnError)

	_, err := applyWithRollback(t.Context(), &reconciler.Changeset{Changes: []reconciler.Change{{
		DestPath: unit, Category: config.CategoryContainer, Action: reconciler.ActionUpdate,
		NewContent: "[Container]\nImage=new\n", ServiceName: "app.service",
	}}}, sys, appliermocks.NewMockPodmanClient(t), nil, nil, "", "")
	require.ErrorIs(t, err, applier.ErrApplyIncomplete)
}

// resolve --host opens its output with one line naming which Agent the files
// are for; user is host.yml's user:, empty for the rootful Host.
func TestRunResolveHeader(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"vps-1":        "# host=vps-1 role=vps machine=vps-1 user=pi listen_port=9417",
		"vps-1-runner": "# host=vps-1-runner role=vps machine=vps-1 user=runner listen_port=9419",
		"vps-1-system": "# host=vps-1-system role=vps machine=vps-1 user= listen_port=9418",
	}
	for host, want := range tests {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			require.NoError(t, runResolve(t.Context(), &out, "../../testdata/example-fleet", host))
			header, _, _ := strings.Cut(out.String(), "\n")
			assert.Equal(t, want, header)
		})
	}
}

// resolve is keyless: a readSopsSecret value prints as <sops-secret>, never
// as its plaintext (the example Fleet encrypts TEST-ONLY values to srv-1).
func TestRunResolveSopsKeyless(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	require.NoError(t, runResolve(t.Context(), &out, "../../testdata/example-fleet", "web-1"))
	assert.Contains(t, out.String(), "password: <sops-secret>")
	assert.NotContains(t, out.String(), "TEST-ONLY-web-app-password")
}

// Cannot use t.Parallel(): t.Setenv() mutates a process-global, so the test
// (and its subtests) must remain serial.
func TestDataDirAndLockPathFromConfig(t *testing.T) { //nolint:paralleltest // see comment above
	home := t.TempDir()
	t.Setenv("HOME", home)

	configuredDataDir := filepath.Join(t.TempDir(), "picolet-data")
	rootlessDataDir := filepath.Join(home, ".local", "share", "picolet")

	tests := map[string]struct {
		cfg         agentcfg.Config
		wantDataDir string
	}{
		"configured data dir": {cfg: agentcfg.Config{DataDir: configuredDataDir, Rootless: true}, wantDataDir: configuredDataDir},
		"rootful default":     {cfg: agentcfg.Config{}, wantDataDir: "/var/lib/picolet"},
		"rootless default":    {cfg: agentcfg.Config{Rootless: true}, wantDataDir: rootlessDataDir},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := dataDirFromConfig(&tc.cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDataDir, got)

			lockPath, err := lockPathFromConfig(&tc.cfg)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(tc.wantDataDir, "reconciliation.lock"), lockPath)
		})
	}
}

// `picolet apply --config` runs the self-restart guard like the Agent: a
// Fleet-rendered Agent config the Agent could not start with fails the apply
// before it connects to systemd or writes anything.
func TestApplyWithConfigRefusesSelfConfigItCouldNotStartWith(t *testing.T) {
	t.Parallel()
	fleet := t.TempDir()
	writeFiles(t, fleet, map[string]string{ //nolint:gosec // G101: an Agent config without credentials
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          "base:\n  services:\n    - picolet-system\n",
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\n",
		"services/picolet-system/picolet-system.container": "[Container]\nImage=ghcr.io/schjan/picolet:latest\n" +
			"Secret=picolet_system_config,target=/etc/picolet/config.yml\n",
		"services/picolet-system/secrets/picolet_system_config.yml": "hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n",
	})
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(configPath, []byte("hostname: test-host\nsecrets_dir: "+filepath.Join(dir, "secrets")+
		"\ndata_dir: "+filepath.Join(dir, "data")+"\n"), 0o600))

	err := runApply(t.Context(), configPath, fleet, "test-host")
	require.ErrorContains(t, err, "agent config check failed")
	require.ErrorContains(t, err, "no_such_key_ever")
}
