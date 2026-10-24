package agent

import (
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/schjan/picolet/internal/testlog"
)

// TestMain routes the process-wide slog default into a recorder and fails the
// package when anything reached it: every Agent a test builds logs through its
// own logger (newTestAgent injects one), and the components the Agent drives
// (config load, resolver, validator, applier, health, rollback, git poller,
// orphan scan) inherit it. A record here is a log line on those paths that
// bypasses the Agent's logger.
func TestMain(m *testing.M) {
	_, global := testlog.New()
	slog.SetDefault(slog.New(global))
	code := m.Run()
	if leaked := global.Records(); code == 0 && len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "%d log record(s) reached the global slog default instead of the Agent's logger:\n%s", len(leaked), global)
		code = 1
	}
	os.Exit(code)
}

// newTestLogger returns a logger for one test. Its records are dumped into
// the test log when the test fails.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	logger, rec := testlog.New()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent log:\n%s", rec)
		}
	})
	return logger
}

// discardLogger is for components a test builds itself (poller, health
// checker, LoadAndResolveHost), so they stay off the global default.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
