package agent

// Pure bookkeeping for the two persisted retry queues: PendingHooks (hooks
// that errored under keep_running) and PendingUnits (managed units whose
// restart failed). These are plain state-transition functions with no Agent
// dependency; the tick loop and ReconcileOnce drive them.

import (
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/state"
)

// mergePendingHooks computes the new PendingHooks map given the previous
// map and the apply result. After the every-tick-retry change a hook either
// runs (success/failed/skipped) or is dropped as stale; it does not stay
// pending across ticks without an attempt. So a hook is removed from
// pending if it appears in AttemptedHookNames (regardless of outcome
// classification — count increments come from PendingHookNames), and added
// if it's a new keep_running failure. Returns nil (not an empty map) when
// empty so omitempty omits the field.
func mergePendingHooks(old map[string]int, result *applier.ApplyResult) map[string]int {
	if len(old) == 0 && len(result.PendingHookNames) == 0 {
		return nil
	}
	attempted := make(map[string]bool, len(result.AttemptedHookNames))
	for _, name := range result.AttemptedHookNames {
		attempted[name] = true
	}
	merged := make(map[string]int, len(old)+len(result.PendingHookNames))
	for name, count := range old {
		if attempted[name] {
			continue
		}
		merged[name] = count
	}
	for _, name := range result.PendingHookNames {
		prev := old[name]
		merged[name] = prev + 1
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// mergePendingUnits computes the new PendingUnits map from the previous map and
// an apply result: units that restarted cleanly are dropped (converged), units
// whose restart failed are added or have their attempt count incremented, and
// every other entry is carried forward unchanged. headSHA records the SHA in
// effect for any failure recorded this call. Timestamps are truncated to whole
// seconds so maps.Equal change-detection is stable. Returns nil (not an empty
// map) when empty so omitempty omits the field.
func mergePendingUnits(old map[string]state.PendingUnit, result *applier.ApplyResult, headSHA string, now time.Time) map[string]state.PendingUnit {
	if len(old) == 0 && len(result.FailedRestartUnits) == 0 {
		return nil
	}
	now = now.Truncate(time.Second)
	merged := make(map[string]state.PendingUnit, len(old)+len(result.FailedRestartUnits))
	maps.Copy(merged, old)
	for _, unit := range result.RestartedUnits {
		delete(merged, unit)
	}
	for _, unit := range result.FailedRestartUnits {
		pu := merged[unit]
		if pu.FirstFailedAt.IsZero() {
			pu.FirstFailedAt = now
		}
		pu.SHA = headSHA
		pu.Attempts++
		pu.LastAttemptAt = now
		merged[unit] = pu
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// prunePendingUnits drops pending-unit records whose unit is no longer managed
// (not present in serviceNames). Mutates and returns pending; returns nil when
// the result is empty so omitempty omits the field.
func prunePendingUnits(pending map[string]state.PendingUnit, serviceNames map[string]string) map[string]state.PendingUnit {
	if len(pending) == 0 {
		return nil
	}
	managed := make(map[string]struct{}, len(serviceNames))
	for _, unit := range serviceNames {
		managed[unit] = struct{}{}
	}
	for unit := range pending {
		if _, ok := managed[unit]; !ok {
			delete(pending, unit)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return pending
}

// pendingUnitAttempts projects PendingUnits to a unit→attempt-count map for the
// picolet_unit_restart_pending metric.
func pendingUnitAttempts(pending map[string]state.PendingUnit) map[string]int {
	if len(pending) == 0 {
		return nil
	}
	out := make(map[string]int, len(pending))
	for unit, pu := range pending {
		out[unit] = pu.Attempts
	}
	return out
}

// pendingHookNames returns the hook names from the pending map in sorted order.
// Sorted output keeps log lines and tests deterministic.
func pendingHookNames(pending map[string]int) []string {
	if len(pending) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(pending))
}

// enforceRetryBudget removes hooks that exceeded their configured max_retries.
// Hooks not found in the config are left untouched (they'll be dropped as stale
// on the next actual retry attempt by RunPendingHooks).
func enforceRetryBudget(pending map[string]int, hooks []config.Hook) map[string]int {
	if len(pending) == 0 {
		return nil
	}
	maxByName := make(map[string]int, len(hooks))
	for _, h := range hooks {
		maxByName[h.Name] = h.MaxRetries
	}
	for name, count := range pending {
		limit, ok := maxByName[name]
		if !ok {
			continue // stale hook name; RunPendingHooks will handle it
		}
		if limit <= 0 {
			limit = config.DefaultMaxRetries
		}
		if count >= limit {
			slog.Error("hook exhausted retry budget, giving up",
				"hook", name, "attempts", count, "max_retries", limit)
			delete(pending, name)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return pending
}
