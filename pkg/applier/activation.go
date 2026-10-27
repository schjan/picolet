package applier

import (
	"context"
	"fmt"
	"time"

	"github.com/schjan/picolet/pkg/state"
)

// RestartCooldown is the minimum time between two restart attempts of a
// unit that failed to restart: the health loop's retry of a failed restart,
// and the execution retry of an Activation Obligation. A permanently broken
// unit must not be hammered every tick.
const RestartCooldown = 5 * time.Minute

// ActivationObligation reports a unit whose required activation the apply
// could not complete: its decision failed (Undecided: nothing was executed),
// or the activation it decided on failed. The caller persists it as a
// state.PendingUnit and retries it with RetryActivation, never through the
// health loop.
type ActivationObligation struct {
	Unit       string
	Activation state.Activation
	Undecided  bool
	Err        error
}

// activationStep is what an activation decision settled on.
type activationStep int

const (
	// stepSkip: activation is intentionally left to a trigger (a .timer, or
	// systemd for an externally activated consumer).
	stepSkip activationStep = iota
	stepRestart
	// stepRestartNoDeps: restart a running consumer without its dependencies.
	stepRestartNoDeps
)

// decideActivation checks unit's current status and decides its activation
// under kind's policy; it also returns the status it decided on. A status
// error decides nothing: the caller must fail closed and never execute on it.
func (a *Applier) decideActivation(ctx context.Context, unit string, kind state.Activation) (activationStep, UnitStatus, error) {
	st, err := a.systemd.GetUnitStatus(ctx, unit)
	if err != nil {
		return stepSkip, st, fmt.Errorf("checking %s before restart: %w", unit, err)
	}
	if kind == state.ActivationRebuiltConsumer {
		if ExternallyActivated(st) {
			return stepSkip, st, nil
		}
		switch st.ActiveState {
		case "active", "activating", "reloading":
			return stepRestartNoDeps, st, nil
		default:
			return stepRestart, st, nil
		}
	}
	if TimerTriggered(st) {
		return stepSkip, st, nil
	}
	return stepRestart, st, nil
}

// executeActivation runs the restart step decided on.
func (a *Applier) executeActivation(ctx context.Context, unit string, step activationStep) error {
	if step == stepRestartNoDeps {
		return a.systemd.RestartUnitIgnoringDependencies(ctx, unit)
	}
	return a.systemd.RestartUnit(ctx, unit)
}

// recordUndecided records a failed activation decision: an error, an
// "error" restart op (not "skipped", which is a deliberate gate), and an
// undecided obligation.
func recordUndecided(unit string, kind state.Activation, err error, result *ApplyResult) {
	result.Errors = append(result.Errors, err)
	result.SystemdUnitOps = append(result.SystemdUnitOps,
		SystemdUnitOp{Unit: unit, Operation: SystemdOpRestart, Result: SystemdOpResultError})
	result.Obligations = append(result.Obligations,
		ActivationObligation{Unit: unit, Activation: kind, Undecided: true, Err: err})
}

// recordFailedActivation records a decided activation that failed: an
// error and an Activation Obligation the caller retries (RetryActivation).
func recordFailedActivation(unit string, kind state.Activation, err error, result *ApplyResult) {
	err = fmt.Errorf("restarting %s: %w", unit, err)
	result.Errors = append(result.Errors, err)
	result.Obligations = append(result.Obligations, ActivationObligation{Unit: unit, Activation: kind, Err: err})
}

// recordSkip records an activation intentionally left to its trigger.
func recordSkip(unit string, result *ApplyResult) {
	result.SystemdUnitOps = append(result.SystemdUnitOps,
		SystemdUnitOp{Unit: unit, Operation: SystemdOpRestart, Result: SystemdOpResultSkipped})
}

// ActivationResult is the outcome of RetryActivation.
type ActivationResult int

const (
	// ActivationUndecided: the decision failed again; nothing was executed.
	ActivationUndecided ActivationResult = iota
	// ActivationSkipped: a trigger owns the activation; the obligation is
	// fulfilled without executing.
	ActivationSkipped
	// ActivationDeferred: the unit must be activated, but the caller did not
	// allow execution yet (its retry cooldown).
	ActivationDeferred
	// ActivationSucceeded: the unit was activated; the obligation is fulfilled.
	ActivationSucceeded
	// ActivationFailed: the activation was executed and failed.
	ActivationFailed
)

// RetryActivation makes a fresh activation decision for an outstanding
// obligation of kind and, when the decision requires activation and
// mayExecute allows it (never in dry-run), executes it. The error is set for
// ActivationUndecided and ActivationFailed. Shares its decision with the
// apply's own activations.
func (a *Applier) RetryActivation(ctx context.Context, unit string, kind state.Activation, mayExecute bool) (ActivationResult, error) {
	mayExecute = mayExecute && !a.dryRun
	step, _, err := a.decideActivation(ctx, unit, kind)
	switch {
	case err != nil:
		a.logger.Warn("activation decision failed, not executing", "unit", unit, "error", err)
		return ActivationUndecided, err
	case step == stepSkip:
		a.logger.Info("activation left to its trigger, obligation fulfilled", "unit", unit)
		return ActivationSkipped, nil
	case !mayExecute:
		return ActivationDeferred, nil
	}
	a.logger.Info("activating unit for outstanding obligation", "unit", unit)
	if err := a.executeActivation(ctx, unit, step); err != nil {
		return ActivationFailed, fmt.Errorf("restarting %s: %w", unit, err)
	}
	return ActivationSucceeded, nil
}
