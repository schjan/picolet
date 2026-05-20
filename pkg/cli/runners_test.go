package cli

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
)

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
