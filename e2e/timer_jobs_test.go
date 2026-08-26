//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// timerJobUnitNames are the raw units this test deploys. Names are unique to this test
// so it never collides with TestE2EPipeline's fixtures on the same bench.
var timerJobUnitNames = []string{
	"e2e-job-ok.service", "e2e-job-ok.timer",
	"e2e-job-fail.service", "e2e-job-fail.timer",
}

// setupTimerJobFleet writes a fleet containing a succeeding and a failing
// timer-triggered one-shot, each with its own fast-firing .timer.
//
// StartLimitIntervalSec=0 keeps the failing job re-runnable: without it systemd's
// start-rate limiter would refuse the repeats this test relies on. The timers fire
// 1s after activation and 5s after each run ends, so a run of either job is
// observable within seconds.
func setupTimerJobFleet(t *testing.T, fleetDir string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(fleetDir, "hosts", "job-host"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(fleetDir, "systemd"), 0o755))

	oneshot := func(desc, exec string) string {
		return fmt.Sprintf(`[Unit]
Description=%s
StartLimitIntervalSec=0

[Service]
Type=oneshot
ExecStart=%s
`, desc, exec)
	}
	timer := func(desc, unit string) string {
		return fmt.Sprintf(`[Unit]
Description=%s

[Timer]
OnActiveSec=1s
OnUnitInactiveSec=5s
AccuracySec=1s
Unit=%s

[Install]
WantedBy=timers.target
`, desc, unit)
	}

	files := map[string]string{
		"fleet.yml": "images: {}\n",
		"assignments.yml": `base:
  systemd:
    - systemd/e2e-job-ok.service
    - systemd/e2e-job-ok.timer
    - systemd/e2e-job-fail.service
    - systemd/e2e-job-fail.timer
roles:
  job: {}
`,
		filepath.Join("hosts", "job-host", "host.yml"): `hostname: job-host
external_hostname: job-host.local
role: job
features: []
`,
		filepath.Join("systemd", "e2e-job-ok.service"):   oneshot("picolet e2e succeeding one-shot", "/bin/true"),
		filepath.Join("systemd", "e2e-job-fail.service"): oneshot("picolet e2e failing one-shot", "/bin/false"),
		filepath.Join("systemd", "e2e-job-ok.timer"):     timer("picolet e2e ok timer", "e2e-job-ok.service"),
		filepath.Join("systemd", "e2e-job-fail.timer"):   timer("picolet e2e fail timer", "e2e-job-fail.service"),
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, name), []byte(content), 0o644))
	}
}

// assertTimerJobsDeployed checks the deploy landed as picolet promises for this
// unit class: both timers enabled and armed, both one-shots present with the
// trigger edge that classifies them, neither one-shot started by picolet.
func assertTimerJobsDeployed(ctx context.Context, t *testing.T, systemd *applier.DBusSystemdManager, systemdDir string) {
	t.Helper()
	for _, unit := range timerJobUnitNames {
		assert.FileExists(t, filepath.Join(systemdDir, unit))
	}
	for _, timerUnit := range []string{"e2e-job-ok.timer", "e2e-job-fail.timer"} {
		st, err := systemd.GetUnitStatus(ctx, timerUnit)
		require.NoError(t, err)
		assert.Equal(t, "enabled", st.UnitFileState, "%s should be enabled", timerUnit)
		assert.Equal(t, "active", st.ActiveState, "%s should be armed", timerUnit)
	}
	for _, svc := range []string{"e2e-job-ok.service", "e2e-job-fail.service"} {
		st, err := systemd.GetUnitStatus(ctx, svc)
		require.NoError(t, err)
		assert.Equal(t, "oneshot", st.ServiceType)
		assert.True(t, applier.TimerTriggeredOneshot(st),
			"%s should classify as a timer-triggered one-shot, got %+v", svc, st)
	}
}

// requireBothJobsRan waits until real systemd reports a finished successful run of
// the good job and a failed run of the bad one.
func requireBothJobsRan(ctx context.Context, t *testing.T, systemd *applier.DBusSystemdManager) {
	t.Helper()
	require.Eventually(t, func() bool {
		ok, err := systemd.GetUnitStatus(ctx, "e2e-job-ok.service")
		if err != nil || ok.LastRunFinishedAt.IsZero() || ok.Result != "success" {
			return false
		}
		bad, err := systemd.GetUnitStatus(ctx, "e2e-job-fail.service")
		return err == nil && bad.ActiveState == "failed" && bad.Result == "exit-code"
	}, 60*time.Second, 2*time.Second, "both timer-triggered one-shots should have run")
}

// TestE2ETimerJobRunMetrics deploys a succeeding and a failing timer-triggered
// one-shot on the real bench, lets their timers fire, and asserts that the
// production health pass + status store + collector turn real systemd properties
// into the last-run/last-success/last-result/last-trigger series.
//
// Runs serially: it deploys raw units into the shared user systemd instance and
// asserts on their run history, which a parallel daemon-reload from another test
// could disturb.
//
//nolint:paralleltest,funlen // shared user systemd instance; sequential staged exercise
func TestE2ETimerJobRunMetrics(t *testing.T) {
	socketPath := podmanSocketPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	fleetDir := filepath.Join(t.TempDir(), "fleet")
	setupTimerJobFleet(t, fleetDir)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	systemdDir := filepath.Join(home, ".config", "systemd", "user")

	podman, err := applier.NewSocketPodmanClient(context.Background(), socketPath)
	require.NoError(t, err)
	systemd, err := applier.NewDBusSystemdManager(ctx, true)
	require.NoError(t, err)
	t.Cleanup(systemd.Close)

	metrics.Register(nil)
	statusStore := status.NewStore()
	stateStore := state.NewStore(filepath.Join(dataDir, "state.json"))

	cfg := &agentcfg.Config{
		Hostname:     "job-host",
		RepoURL:      "file://" + fleetDir,
		PollInterval: time.Minute,
		MetricsPort:  0,
		SecretsDir:   filepath.Join(fleetDir, "secrets"),
		Rootless:     true,
	}
	a := agent.New(cfg,
		agent.WithRepoPath(fleetDir),
		agent.WithFileWriter(applier.NewAtomicFileWriter()),
		agent.WithPodman(podman),
		agent.WithSystemd(systemd),
		agent.WithLockPath(filepath.Join(dataDir, "reconciliation.lock")),
		agent.WithStatePath(filepath.Join(dataDir, "state.json")),
		agent.WithStatusStore(statusStore),
	)
	healthChecker := health.New(systemd)

	// Registered before the deploy so the units are torn down even if a stage below
	// fails: stop + disable the timers, then remove the unit files and reload.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, unit := range timerJobUnitNames {
			_ = systemd.StopUnit(cctx, unit)
			_ = systemd.DisableUnit(cctx, unit)
			_ = os.Remove(filepath.Join(systemdDir, unit))
		}
		_ = systemd.DaemonReload(cctx)
	})

	t.Run("deploy", func(t *testing.T) {
		result, err := a.ReconcileOnce(ctx, "timer-jobs-sha", state.NewState(), stateStore)
		require.NoError(t, err)
		require.NotNil(t, result.ApplyResult)
		assert.Empty(t, result.ApplyResult.Errors)
		assertTimerJobsDeployed(ctx, t, systemd, systemdDir)
	})

	t.Run("both_jobs_run", func(t *testing.T) {
		requireBothJobsRan(ctx, t, systemd)
	})

	t.Run("store_records_runs", func(t *testing.T) {
		st, err := stateStore.Load()
		require.NoError(t, err)
		hr, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		assert.ElementsMatch(t, timerJobUnitNames, hr.TimerJobs,
			"both one-shots and both timers carry run bookkeeping")
		assert.NotContains(t, hr.Restarted, "e2e-job-fail.service",
			"a timer-triggered one-shot is systemd's to re-run")
		agent.RecordTimerJobRuns(statusStore, hr)

		runs := statusStore.Snapshot().Runs
		okRun := runs["e2e-job-ok.service"]
		assert.False(t, okRun.StartedAt.IsZero(), "the succeeding job must report a last run")
		assert.Equal(t, "success", okRun.Result)
		assert.Equal(t, okRun.FinishedAt, okRun.SucceededAt,
			"a successful run's finish time is its last success")

		for _, timerUnit := range []string{"e2e-job-ok.timer", "e2e-job-fail.timer"} {
			assert.False(t, runs[timerUnit].TriggeredAt.IsZero(),
				"%s should report a last trigger", timerUnit)
			assert.True(t, runs[timerUnit].StartedAt.IsZero(),
				"a timer's own activation is not a run of the job it fires")
			assert.Empty(t, runs[timerUnit].Result, "a timer carries no service result")
		}
	})

	t.Run("series_exported", func(t *testing.T) {
		c := metrics.NewUnitRunCollector(statusStore)
		assert.Equal(t, 2, testutil.CollectAndCount(c, "picolet_unit_last_run_timestamp_seconds"))
		assert.Equal(t, 1, testutil.CollectAndCount(c, "picolet_unit_last_success_timestamp_seconds"),
			"only the succeeding job has a last-success series")
		assert.Equal(t, 2, testutil.CollectAndCount(c, "picolet_unit_last_result"))
		assert.Equal(t, 2, testutil.CollectAndCount(c, "picolet_timer_last_trigger_timestamp_seconds"))
	})

	// Both timers re-invoke their job on their own. Re-observing across a *second*
	// completed run of each is the real test of the derivation: the succeeding job's
	// last-success must move forward, and the failing job must still have none —
	// with real systemd resetting Result= to "success" at every start in between.
	t.Run("timer_keeps_reinvoking", func(t *testing.T) {
		before := statusStore.Snapshot().Runs
		okBefore := before["e2e-job-ok.service"]
		failBefore := before["e2e-job-fail.service"]
		require.False(t, okBefore.SucceededAt.IsZero(), "the earlier pass must have recorded a success")

		// Wait for both jobs to have *finished* a further run, not merely started one:
		// last-success can only advance once the new run has completed.
		require.Eventually(t, func() bool {
			ok, err := systemd.GetUnitStatus(ctx, "e2e-job-ok.service")
			if err != nil || !ok.LastRunFinishedAt.After(okBefore.FinishedAt) {
				return false
			}
			bad, err := systemd.GetUnitStatus(ctx, "e2e-job-fail.service")
			return err == nil && bad.LastRunFinishedAt.After(failBefore.FinishedAt)
		}, 60*time.Second, 2*time.Second, "both timers should re-invoke their job on their own")

		st, err := stateStore.Load()
		require.NoError(t, err)
		hr, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		agent.RecordTimerJobRuns(statusStore, hr)

		after := statusStore.Snapshot().Runs
		okAfter := after["e2e-job-ok.service"]
		assert.True(t, okAfter.SucceededAt.After(okBefore.SucceededAt),
			"a further successful run must advance last success (was %s, now %s)",
			okBefore.SucceededAt, okAfter.SucceededAt)
		assert.Equal(t, okAfter.FinishedAt, okAfter.SucceededAt,
			"the succeeding job's last success is the finish of its latest run")
		failAfter := after["e2e-job-fail.service"]
		assert.True(t, failAfter.StartedAt.After(failBefore.StartedAt), "the failing job ran again")
		assert.Equal(t, "exit-code", failAfter.Result)
		assert.True(t, failAfter.SucceededAt.IsZero(),
			"the failing job must never acquire a last success")
	})

	t.Run("removal_prunes_records", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(fleetDir, "assignments.yml"),
			[]byte("base: {}\nroles:\n  job: {}\n"), 0o644))
		st, err := stateStore.Load()
		require.NoError(t, err)
		_, err = a.ReconcileOnce(ctx, "timer-jobs-removed-sha", st, stateStore)
		require.NoError(t, err)

		st, err = stateStore.Load()
		require.NoError(t, err)
		assert.Empty(t, st.ServiceNames, "the fleet no longer manages any unit")
		hr, err := healthChecker.Enforce(ctx, st)
		require.NoError(t, err)
		agent.RecordTimerJobRuns(statusStore, hr)
		assert.Empty(t, statusStore.Snapshot().Runs,
			"records are pruned once their units leave the fleet")
	})
}
