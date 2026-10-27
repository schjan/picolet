package agent

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/state"
)

const (
	jobUnit      = "job.service"
	jobOneshot   = "[Container]\nImage=docker.io/library/alpine:3\n\n[Service]\nType=oneshot\n"
	jobOneshotV2 = jobOneshot + "Environment=V=2\n"
)

var (
	jobEligible  = applier.UnitStatus{ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot"}
	jobTimerJob  = applier.UnitStatus{ActiveState: "inactive", SubState: "dead", ServiceType: "oneshot", TriggeredBy: []string{"job.timer"}}
	errStatusErr = assert.AnError
)

// obligationHarness drives Agents over a Fleet whose only file is a Quadlet
// one-shot, with a real state store. Every reconcile builds a fresh Agent on
// the same store (an Agent restart), so nothing carries over in memory.
// GetUnitStatus answers jobStatus/jobStatusErr; RestartUnit answers
// restartErr and counts executions in restarts.
type obligationHarness struct {
	t       *testing.T
	repoDir string
	store   *state.Store
	sys     *appliermocks.MockSystemdManager
	fw      *appliermocks.MockFileWriter
	cfg     *agentcfg.Config

	jobStatus    applier.UnitStatus
	jobStatusErr error
	restartErr   error
	restarts     int
}

func newObligationHarness(t *testing.T, pollInterval time.Duration) *obligationHarness {
	t.Helper()
	sys, _, fw := newBareMocks(t)
	h := &obligationHarness{
		t:       t,
		repoDir: t.TempDir(),
		store:   state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		sys:     sys,
		fw:      fw,
		cfg:     &agentcfg.Config{Hostname: "test-host", SecretsDir: t.TempDir(), PollInterval: pollInterval},
	}
	writeTestFile(t, h.repoDir, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
	writeTestFile(t, h.repoDir, "hosts/test-host/host.yml", "hostname: test-host\nrole: server\nfeatures: []\n")
	h.assignJob(jobOneshot)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil).Maybe()
	fw.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(nil).Maybe()
	fw.EXPECT().Remove(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StopUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().GetUnitStatus(mock.Anything, jobUnit).RunAndReturn(func(context.Context, string) (applier.UnitStatus, error) {
		return h.jobStatus, h.jobStatusErr
	}).Maybe()
	sys.EXPECT().RestartUnit(mock.Anything, jobUnit).RunAndReturn(func(context.Context, string) error {
		h.restarts++
		return h.restartErr
	}).Maybe()
	return h
}

// assignJob makes the Fleet deploy the one-shot with content.
func (h *obligationHarness) assignJob(content string) {
	writeTestFile(h.t, h.repoDir, "assignments.yml", "base:\n  paths:\n    - quadlets/job.container\n")
	writeTestFile(h.t, h.repoDir, "quadlets/job.container", content)
}

// unassignJob removes the one-shot from the Fleet.
func (h *obligationHarness) unassignJob() {
	writeTestFile(h.t, h.repoDir, "assignments.yml", "base:\n  paths: []\n")
}

// reconcile runs one reconciliation of sha on a fresh Agent.
func (h *obligationHarness) reconcile(sha string) error {
	h.t.Helper()
	a := newTestAgent(h.t, h.cfg, WithRepoPath(h.repoDir), WithSystemd(h.sys),
		WithPodman(appliermocks.NewMockPodmanClient(h.t)), WithFileWriter(h.fw))
	_, err := a.ReconcileOnce(h.t.Context(), sha, h.load(), h.store)
	return err
}

func (h *obligationHarness) load() *state.State {
	h.t.Helper()
	st, err := h.store.Load()
	require.NoError(h.t, err)
	return st
}

// job returns the job's pending record.
func (h *obligationHarness) job() state.PendingUnit {
	h.t.Helper()
	pu, ok := h.load().PendingUnits[jobUnit]
	require.True(h.t, ok, "job has no pending record")
	return pu
}

// age moves the job's last check and last attempt back by d, as if d passed.
func (h *obligationHarness) age(d time.Duration) {
	h.t.Helper()
	st := h.load()
	pu := st.PendingUnits[jobUnit]
	if !pu.LastCheckAt.IsZero() {
		pu.LastCheckAt = pu.LastCheckAt.Add(-d)
	}
	if !pu.LastAttemptAt.IsZero() {
		pu.LastAttemptAt = pu.LastAttemptAt.Add(-d)
	}
	st.PendingUnits[jobUnit] = pu
	require.NoError(h.t, h.store.Save(st))
}

// A failed gate writes the file but never runs the one-shot: the undecided
// obligation is persisted with the applied files and SHA, and reconciliation
// stays incomplete (no last-successful timestamp).
func TestObligationFailedGatePersistsIncomplete(t *testing.T) {
	t.Parallel()
	h := newObligationHarness(t, time.Minute)
	h.jobStatusErr = errStatusErr

	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)

	st := h.load()
	assert.Equal(t, "sha-1", st.AppliedSHA)
	assert.Contains(t, slices.Collect(maps.Values(st.ServiceNames)), jobUnit, "the file is recorded as applied")
	assert.True(t, st.LastSuccessfulReconciliationAt.IsZero())
	pu := h.job()
	assert.Equal(t, state.PendingActivationUndecided, pu.Reason)
	assert.Equal(t, "sha-1", pu.SHA)
	assert.NotEmpty(t, pu.ContentHash)
	assert.Equal(t, 1, pu.Checks)
	assert.Contains(t, pu.LastError, errStatusErr.Error())
	assert.Zero(t, h.restarts)
}

// Same-SHA, empty-diff reconciliations retry the decision once per poll
// interval, without a retry limit: an extra reconcile within the interval
// (a webhook) does not check again. Recovery to a timer-triggered status
// fulfils the obligation by intentional skip: zero executions, and the
// reconciliation becomes successful.
func TestObligationRetriedEachIntervalUntilTimerSkip(t *testing.T) {
	t.Parallel()
	h := newObligationHarness(t, time.Minute)
	h.jobStatusErr = errStatusErr
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)

	// Within the interval: no new check.
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
	assert.Equal(t, 1, h.job().Checks)

	for want := 2; want <= 5; want++ {
		h.age(time.Minute)
		require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
		assert.Equal(t, want, h.job().Checks)
	}
	assert.Equal(t, "sha-1", h.load().AppliedSHA)

	h.age(time.Minute)
	h.jobStatus, h.jobStatusErr = jobTimerJob, nil
	require.NoError(t, h.reconcile("sha-1"))

	st := h.load()
	assert.Empty(t, st.PendingUnits)
	assert.False(t, st.LastSuccessfulReconciliationAt.IsZero())
	assert.Zero(t, h.restarts)
}

// Recovery to an eligible non-timer status executes the one-shot; a failed
// execution keeps the obligation and waits out the restart cooldown (the
// decision is still checked each interval), and only a terminal success
// clears it.
func TestObligationExecutesAfterCooldownUntilSuccess(t *testing.T) {
	t.Parallel()
	h := newObligationHarness(t, time.Minute)
	h.jobStatusErr = errStatusErr
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)

	h.age(time.Minute)
	h.jobStatus, h.jobStatusErr = jobEligible, nil
	h.restartErr = errStatusErr
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
	assert.Equal(t, 1, h.restarts)
	pu := h.job()
	assert.Equal(t, state.PendingActivationFailed, pu.Reason)
	assert.Equal(t, 1, pu.Attempts)

	// Next interval, inside the cooldown: checked, not executed.
	h.age(time.Minute)
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
	assert.Equal(t, 1, h.restarts)
	assert.Equal(t, 3, h.job().Checks)

	h.age(applier.RestartCooldown)
	h.restartErr = nil
	require.NoError(t, h.reconcile("sha-1"))
	assert.Equal(t, 2, h.restarts)
	assert.Empty(t, h.load().PendingUnits)
	assert.False(t, h.load().LastSuccessfulReconciliationAt.IsZero())
}

// New content supersedes an outstanding obligation: its apply decides afresh
// (here: a timer now owns the job, so it is skipped) and the old obligation
// is never executed.
func TestObligationSupersededByNewContent(t *testing.T) {
	t.Parallel()
	h := newObligationHarness(t, time.Minute)
	h.jobStatusErr = errStatusErr
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
	oldHash := h.job().ContentHash

	// Failed again for new content: a fresh record for the new content.
	h.assignJob(jobOneshotV2)
	require.ErrorIs(t, h.reconcile("sha-2"), applier.ErrApplyIncomplete)
	pu := h.job()
	assert.NotEqual(t, oldHash, pu.ContentHash)
	assert.Equal(t, "sha-2", pu.SHA)
	assert.Equal(t, 1, pu.Checks)

	h.assignJob(jobOneshot)
	h.jobStatus, h.jobStatusErr = jobTimerJob, nil
	require.NoError(t, h.reconcile("sha-3"))
	assert.Empty(t, h.load().PendingUnits)
	assert.Zero(t, h.restarts)
}

// An unrelated commit (identical rendered content) keeps the obligation and
// its history; removing the unit from the Fleet cancels it without
// executing.
func TestObligationKeptByUnrelatedCommitCancelledByRemoval(t *testing.T) {
	t.Parallel()
	h := newObligationHarness(t, time.Minute)
	h.jobStatusErr = errStatusErr
	require.ErrorIs(t, h.reconcile("sha-1"), applier.ErrApplyIncomplete)
	before := h.job()

	require.ErrorIs(t, h.reconcile("sha-2"), applier.ErrApplyIncomplete)
	after := h.job()
	assert.Equal(t, before.ContentHash, after.ContentHash)
	assert.True(t, before.FirstFailedAt.Equal(after.FirstFailedAt))
	assert.Equal(t, "sha-2", h.load().AppliedSHA)
	assert.True(t, h.load().LastSuccessfulReconciliationAt.IsZero())

	h.unassignJob()
	require.NoError(t, h.reconcile("sha-3"))
	assert.Empty(t, h.load().PendingUnits)
	assert.Zero(t, h.restarts)
}

// On the Agent loop, an obligation passes through health untouched (the job
// looks active, which would clear an ordinary pending record), a paused
// Agent neither checks nor executes it, and the next unpaused tick
// reconciles and executes it once.
func TestObligationTickPauseAndHealth(t *testing.T) {
	t.Parallel()
	bareDir, _ := initFleetRepo(t, map[string]string{
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          "base:\n  paths:\n    - quadlets/job.container\n",
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\n",
		"quadlets/job.container":   jobOneshot,
	})
	h := newLoopHarness(t, bareDir)
	h.mocks = mocksNoop | mockRestarts
	old := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
	ob := state.PendingUnit{SHA: "x", Reason: state.PendingActivationUndecided, Checks: 1, LastCheckAt: old, FirstFailedAt: old}
	h.seedState(t, true, func(st *state.State) {
		st.LastSuccessfulReconciliationAt = time.Time{}
		st.PendingUnits = map[string]state.PendingUnit{jobUnit: ob}
	})

	h.a.paused.Store(true)
	require.NoError(t, h.tick(t))
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, map[string]state.PendingUnit{jobUnit: ob}, st.PendingUnits)
	assert.Empty(t, h.restarts)

	h.a.paused.Store(false)
	require.NoError(t, h.tick(t))
	st, err = h.store.Load()
	require.NoError(t, err)
	assert.Empty(t, st.PendingUnits)
	assert.Equal(t, []string{jobUnit}, h.restarts)
	assert.False(t, st.LastSuccessfulReconciliationAt.IsZero())
}

// A failed ordinary restart of a unit with an obligation (its new content is
// no longer a one-shot) replaces the obligation with an ordinary pending
// record, which the health loop owns.
func TestRecordApplyResultFailedRestartReplacesObligation(t *testing.T) {
	t.Parallel()
	st := state.NewState()
	st.PendingUnits = map[string]state.PendingUnit{
		jobUnit: {Reason: state.PendingActivationFailed, Attempts: 3, Checks: 4, LastError: "old"},
	}
	changeset := &reconciler.Changeset{Changes: []reconciler.Change{{
		DestPath: "/etc/containers/systemd/picolet/job.container", Category: config.CategoryContainer,
		Action: reconciler.ActionUpdate, NewContent: "[Container]\nImage=x\n", ServiceName: jobUnit,
	}}}

	RecordApplyResult(st, changeset, &applier.ApplyResult{FailedRestartUnits: []string{jobUnit}}, "sha-2", time.Now())

	pu := st.PendingUnits[jobUnit]
	assert.False(t, pu.IsObligation())
	assert.Equal(t, 1, pu.Attempts)
	assert.Equal(t, "sha-2", pu.SHA)
}
