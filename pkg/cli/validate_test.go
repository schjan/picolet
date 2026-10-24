package cli

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
)

// writeWarningFleet writes a one-Host Fleet that validates but makes the
// resolver warn: the Host's role has no assignments group.
func writeWarningFleet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"fleet.yml": "ports:\n  picolet_system_metrics: 9100\n",
		"assignments.yml": `base:
  paths:
    - quadlets/app.network
`,
		"hosts/h1/host.yml":    "hostname: h1\nrole: lonely\nmachine: m1\n",
		"quadlets/app.network": "[Network]\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

func TestRunValidateLogsThroughGivenLogger(t *testing.T) {
	t.Parallel()
	logger, logs := testlog.New()

	require.NoError(t, runValidate(t.Context(), writeWarningFleet(t), logger))

	assert.True(t, logs.Has(slog.LevelInfo, "host validated"), logs.String())
	assert.True(t, logs.Has(slog.LevelWarn, "no assignments for role"), logs.String())
}

// validateGuardEnv marks the child process of
// TestRunValidateLeavesGlobalLoggerAlone.
const validateGuardEnv = "PICOLET_TEST_VALIDATE_GLOBAL_GUARD"

// A validate run with its own logger writes nothing to the global slog
// default. The global default is process state, so the check runs in a child
// process where swapping it cannot disturb parallel tests.
func TestRunValidateLeavesGlobalLoggerAlone(t *testing.T) {
	t.Parallel()
	if os.Getenv(validateGuardEnv) == "1" {
		_, global := testlog.New()
		slog.SetDefault(slog.New(global))
		logger, logs := testlog.New()
		require.NoError(t, runValidate(t.Context(), writeWarningFleet(t), logger))
		require.NotEmpty(t, logs.Records(), "the run logs through its own logger")
		require.Empty(t, global.Records(), "records reached the global slog default:\n%s", global)
		return
	}

	//nolint:gosec // re-executes this test binary with a fixed test filter
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunValidateLeavesGlobalLoggerAlone$", "-test.count=1")
	cmd.Env = append(os.Environ(), validateGuardEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "child validate run:\n%s", out)
}
