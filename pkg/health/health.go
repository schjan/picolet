package health

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

// passiveUnitExts are systemd activator unit types with no running process
// (timers, sockets, targets, paths). They sit in active (waiting) at rest and
// have no meaningful "restart to recover" semantics, so health-enforce reports
// their state but never restarts them.
var passiveUnitExts = map[string]struct{}{
	".timer":  {},
	".socket": {},
	".target": {},
	".path":   {},
}

// isPassiveUnit reports whether a unit name is a passive activator unit.
func isPassiveUnit(unit string) bool {
	_, ok := passiveUnitExts[filepath.Ext(unit)]
	return ok
}

const restartCooldown = 5 * time.Minute

// CheckResult holds the outcome of a health enforcement pass.
type CheckResult struct {
	Healthy   []string
	Unhealthy []string
	Inactive  []string // oneshots, timer-activated services between runs
	Restarted []string
	Skipped   []string
	Errors    []error
	Statuses  map[string]applier.UnitStatus // all successfully queried units
}

// AllFailed reports whether every unit check errored and none succeeded.
// Returns false when no units were checked (empty state).
func (r *CheckResult) AllFailed() bool {
	totalChecked := len(r.Healthy) + len(r.Unhealthy) + len(r.Inactive) + len(r.Errors)
	return totalChecked > 0 && len(r.Errors) == totalChecked
}

// Checker enforces that managed systemd units are active.
type Checker struct {
	systemd applier.SystemdManager
	// Accessed only by the agent tick loop, which runs serially.
	lastRestart map[string]time.Time
}

// New creates a new health Checker.
func New(systemd applier.SystemdManager) *Checker {
	return &Checker{
		systemd:     systemd,
		lastRestart: make(map[string]time.Time),
	}
}

// Enforce checks all managed units and restarts any that are failed, subject to
// a per-unit restart cooldown. It also maintains st.PendingUnits: units observed
// healthy are cleared, units whose restart fails are recorded, and records for
// units no longer managed are pruned. The caller persists st after Enforce.
func (c *Checker) Enforce(ctx context.Context, st *state.State) (*CheckResult, error) {
	result := &CheckResult{Statuses: make(map[string]applier.UnitStatus)}

	// Derive unique unit names from the service names map
	units := make(map[string]bool)
	for _, unitName := range st.ServiceNames {
		if unitName != "" {
			units[unitName] = true
		}
	}
	for unit := range c.lastRestart {
		if !units[unit] {
			delete(c.lastRestart, unit)
		}
	}
	// Restore the restart cooldown across an agent restart (a fresh Checker has
	// an empty lastRestart map) and prune pending records for units that left
	// the fleet — health-enforce, unlike the apply path, has no other signal
	// that a unit was removed.
	for unit, pu := range st.PendingUnits {
		if !units[unit] {
			delete(st.PendingUnits, unit)
			continue
		}
		if pu.LastAttemptAt.After(c.lastRestart[unit]) {
			c.lastRestart[unit] = pu.LastAttemptAt
		}
	}

	for unit := range units {
		c.enforceUnit(ctx, unit, st, result)
	}

	if len(st.PendingUnits) == 0 {
		st.PendingUnits = nil
	}
	return result, nil
}

func (c *Checker) enforceUnit(ctx context.Context, unit string, st *state.State, result *CheckResult) {
	status, err := c.systemd.GetUnitStatus(ctx, unit)
	if err != nil {
		slog.Warn("health check failed", "unit", unit, "error", err)
		result.Errors = append(result.Errors, err)
		return
	}
	result.Statuses[unit] = status

	// Passive activator units (.timer/.socket/.target/.path) are never restarted:
	// a timer at rest is active (waiting), and an enabled-but-inactive timer is
	// expected between runs. Report status, clear any stale pending record, return.
	if isPassiveUnit(unit) {
		if status.ActiveState == "active" || status.ActiveState == "activating" {
			result.Healthy = append(result.Healthy, unit)
		} else {
			result.Inactive = append(result.Inactive, unit)
		}
		delete(st.PendingUnits, unit)
		return
	}

	switch status.ActiveState {
	case "active", "activating":
		// Covers: running daemons (sub_state=running), successful oneshots
		// (sub_state=exited), socket-activated units (sub_state=waiting).
		result.Healthy = append(result.Healthy, unit)
		// Unit converged — clear any pending restart-failure record.
		delete(st.PendingUnits, unit)
	case "inactive", "deactivating", "reloading", "maintenance":
		// Expected for timer-activated services between runs, completed oneshots,
		// or units in condition-check failure. Do not restart. health-enforce
		// no longer retries the unit in this state, so clear any pending
		// restart-failure record — keeping it would loop retry_pending forever.
		result.Inactive = append(result.Inactive, unit)
		delete(st.PendingUnits, unit)
	default:
		// "failed" and any unexpected state — restart conservatively.
		slog.Warn("unit unhealthy", "unit", unit, "active_state", status.ActiveState, "sub_state", status.SubState)
		result.Unhealthy = append(result.Unhealthy, unit)
		c.maybeRestart(ctx, unit, st, result)
	}
}

func (c *Checker) maybeRestart(ctx context.Context, unit string, st *state.State, result *CheckResult) {
	if last, ok := c.lastRestart[unit]; ok {
		elapsed := time.Since(last)
		if elapsed < restartCooldown {
			slog.Info("skipping restart, cooldown active", "unit", unit, "cooldown_remaining", (restartCooldown - elapsed).Round(time.Second))
			result.Skipped = append(result.Skipped, unit)
			return
		}
	}
	// Record the attempt before the call so a failed restart also starts a
	// cooldown — a permanently-broken unit must not be hammered every tick.
	now := time.Now()
	c.lastRestart[unit] = now
	if err := c.systemd.RestartUnit(ctx, unit); err != nil {
		slog.Error("restart failed", "unit", unit, "error", err)
		result.Errors = append(result.Errors, err)
		recordPendingUnit(st, unit, now)
		return
	}
	result.Restarted = append(result.Restarted, unit)
}

// recordPendingUnit adds or increments a unit's pending restart-failure record.
// Timestamps are truncated to whole seconds so maps.Equal change-detection by
// the agent tick is stable across the persist/reload cycle.
func recordPendingUnit(st *state.State, unit string, now time.Time) {
	if st.PendingUnits == nil {
		st.PendingUnits = make(map[string]state.PendingUnit)
	}
	now = now.Truncate(time.Second)
	pu := st.PendingUnits[unit]
	if pu.FirstFailedAt.IsZero() {
		pu.FirstFailedAt = now
	}
	pu.SHA = st.AppliedSHA
	pu.Attempts++
	pu.LastAttemptAt = now
	st.PendingUnits[unit] = pu
}
