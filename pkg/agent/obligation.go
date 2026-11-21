package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/state"
)

// RecordApplyResult records an apply of changeset (headSHA) in st: the
// Managed Files (UpdateState) and the pending units. Units that restarted
// cleanly or whose activation was intentionally left to a trigger are
// cleared, failed restarts and Activation Obligations recorded, and the
// records pruned against the rebuilt ServiceNames, so a unit removed from
// the Fleet drops its record. Shared by the Agent and `picolet apply`; the
// Agent adds its pending hooks.
func RecordApplyResult(st *state.State, changeset *reconciler.Changeset, result *applier.ApplyResult, headSHA string, now time.Time) {
	UpdateState(st, changeset)
	pending := mergePendingUnits(st.PendingUnits, result, headSHA, now)
	pending = mergeObligations(pending, result, headSHA, now, unitContentHashes(st))
	st.PendingUnits = prunePendingUnits(pending, st.ServiceNames)
}

// mergeObligations folds an apply's activation outcomes into pending: a
// fresh intentional skip fulfils a unit's record, and each reported
// obligation is recorded. An obligation for content other than the
// recorded one (or replacing a non-obligation record) supersedes the old
// record and starts a new history; one for the same content continues it.
// Timestamps are truncated to whole seconds (see mergePendingUnits).
func mergeObligations(pending map[string]state.PendingUnit, result *applier.ApplyResult, headSHA string, now time.Time, hashes map[string]string) map[string]state.PendingUnit {
	skipped := result.SkippedRestarts()
	if len(result.Obligations) == 0 && (len(pending) == 0 || len(skipped) == 0) {
		return pending
	}
	if pending == nil {
		pending = make(map[string]state.PendingUnit, len(result.Obligations))
	}
	for _, unit := range skipped {
		delete(pending, unit)
	}
	now = now.Truncate(time.Second)
	for _, ob := range result.Obligations {
		pending[ob.Unit] = recordObligation(pending[ob.Unit], ob, headSHA, now, hashes[ob.Unit])
	}
	if len(pending) == 0 {
		return nil
	}
	return pending
}

// recordObligation updates a unit's record pu with an apply's obligation
// ob for content hash: a new record unless pu is an obligation for the same
// content.
func recordObligation(pu state.PendingUnit, ob applier.ActivationObligation, headSHA string, now time.Time, hash string) state.PendingUnit {
	if !pu.IsObligation() || pu.ContentHash != hash {
		pu = state.PendingUnit{FirstFailedAt: now, ContentHash: hash}
	}
	pu.SHA = headSHA
	pu.Activation = ob.Activation
	pu.Checks++
	pu.LastCheckAt = now
	pu.LastError = errorText(ob.Err)
	pu.Reason = state.PendingActivationUndecided
	if !ob.Undecided {
		pu.Reason = state.PendingActivationFailed
		pu.Attempts++
		pu.LastAttemptAt = now
	}
	return pu
}

// unitContentHashes maps each managed unit to the hash of the Managed Files
// it is generated from: the applied content an obligation is about.
func unitContentHashes(st *state.State) map[string]string {
	byUnit := make(map[string][]string, len(st.ServiceNames))
	for path, unit := range st.ServiceNames {
		byUnit[unit] = append(byUnit[unit], st.ManagedFiles[path].Hash)
	}
	hashes := make(map[string]string, len(byUnit))
	for unit, hs := range byUnit {
		slices.Sort(hs)
		hashes[unit] = strings.Join(hs, ",")
	}
	return hashes
}

// RetryObligations retries every outstanding Activation Obligation in st
// whose last decision is at least cadence old (0: every one): a fresh
// decision, then execution when the decision requires it and the unit's
// restart cooldown (applier.RestartCooldown since its last execution) has
// passed. A fulfilled obligation (intentional skip, terminal success) is
// removed; an undecided or failed one is kept with its counters updated.
// Call it only once the current desired state is resolved and applied, so a
// superseded or removed obligation is never executed. Reports whether st
// changed.
func RetryObligations(ctx context.Context, app *applier.Applier, st *state.State, now time.Time, cadence time.Duration, logger *slog.Logger) bool {
	now = now.Truncate(time.Second)
	changed := false
	for _, unit := range slices.Sorted(maps.Keys(st.PendingUnits)) {
		pu := st.PendingUnits[unit]
		if !pu.IsObligation() || (!pu.LastCheckAt.IsZero() && now.Sub(pu.LastCheckAt) < cadence) {
			continue
		}
		mayExecute := pu.LastAttemptAt.IsZero() || now.Sub(pu.LastAttemptAt) >= applier.RestartCooldown
		kind := cmp.Or(pu.Activation, state.ActivationRestart)
		res, err := app.RetryActivation(ctx, unit, kind, mayExecute)
		changed = true
		if res == applier.ActivationSkipped || res == applier.ActivationSucceeded {
			delete(st.PendingUnits, unit)
			continue
		}
		pu = retriedObligation(pu, res, err, now)
		st.PendingUnits[unit] = pu
		if res == applier.ActivationDeferred {
			logger.Info("activation obligation waiting for its restart cooldown", "unit", unit,
				"next_attempt_at", pu.LastAttemptAt.Add(applier.RestartCooldown))
		}
		logger.Warn("activation obligation outstanding", "unit", unit, "reason", pu.Reason,
			"checks", pu.Checks, "attempts", pu.Attempts, "last_error", pu.LastError)
	}
	if len(st.PendingUnits) == 0 {
		st.PendingUnits = nil
	}
	return changed
}

// retriedObligation records on pu a retry at now that did not fulfil it:
// undecided again, deferred (decided, waiting out the cooldown after a
// failed execution) or failed.
func retriedObligation(pu state.PendingUnit, res applier.ActivationResult, err error, now time.Time) state.PendingUnit {
	pu.Checks++
	pu.LastCheckAt = now
	switch res {
	case applier.ActivationUndecided:
		pu.Reason = state.PendingActivationUndecided
		pu.LastError = errorText(err)
	case applier.ActivationFailed:
		pu.Reason = state.PendingActivationFailed
		pu.Attempts++
		pu.LastAttemptAt = now
		pu.LastError = errorText(err)
	default:
		pu.Reason = state.PendingActivationFailed
	}
	return pu
}

// PendingConvergenceError is the one completion predicate: nil when nothing
// is pending in st, else an applier.ErrApplyIncomplete-wrapped error naming
// the outstanding Activation Obligations, failed unit restarts and pending
// hooks. A reconciliation is successful only when it returns nil.
func PendingConvergenceError(st *state.State) error {
	var obligations, restarts []string
	for unit, pu := range st.PendingUnits {
		if pu.IsObligation() {
			obligations = append(obligations, unit)
		} else {
			restarts = append(restarts, unit)
		}
	}
	var errs []error
	if len(obligations) > 0 {
		slices.Sort(obligations)
		errs = append(errs, fmt.Errorf("activation pending: %v", obligations))
	}
	if len(restarts) > 0 {
		slices.Sort(restarts)
		errs = append(errs, fmt.Errorf("units failed to restart: %v", restarts))
	}
	if len(st.PendingHooks) > 0 {
		errs = append(errs, fmt.Errorf("hooks pending: %v", pendingHookNames(st.PendingHooks)))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", applier.ErrApplyIncomplete, errors.Join(errs...))
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
