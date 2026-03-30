package health

import (
	"context"
	"log/slog"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

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
	systemd     applier.SystemdManager
	lastRestart map[string]time.Time
}

// New creates a new health Checker.
func New(systemd applier.SystemdManager) *Checker {
	return &Checker{
		systemd:     systemd,
		lastRestart: make(map[string]time.Time),
	}
}

// Enforce checks all managed units and restarts any that are failed,
// subject to a per-unit restart cooldown.
func (c *Checker) Enforce(ctx context.Context, st *state.State) (*CheckResult, error) {
	result := &CheckResult{Statuses: make(map[string]applier.UnitStatus)}

	// Derive unique unit names from the service names map
	units := make(map[string]bool)
	for _, unitName := range st.ServiceNames {
		if unitName != "" {
			units[unitName] = true
		}
	}

	for unit := range units {
		c.enforceUnit(ctx, unit, result)
	}

	return result, nil
}

func (c *Checker) enforceUnit(ctx context.Context, unit string, result *CheckResult) {
	status, err := c.systemd.GetUnitStatus(ctx, unit)
	if err != nil {
		slog.Warn("health check failed", "unit", unit, "error", err)
		result.Errors = append(result.Errors, err)
		return
	}
	result.Statuses[unit] = status

	switch status.ActiveState {
	case "active", "activating":
		// Covers: running daemons (sub_state=running), successful oneshots
		// (sub_state=exited), socket-activated units (sub_state=waiting).
		result.Healthy = append(result.Healthy, unit)
	case "inactive", "deactivating", "reloading", "maintenance":
		// Expected for timer-activated services between runs, completed oneshots,
		// or units in condition-check failure. Do not restart.
		result.Inactive = append(result.Inactive, unit)
	default:
		// "failed" and any unexpected state — restart conservatively.
		slog.Warn("unit unhealthy", "unit", unit, "active_state", status.ActiveState, "sub_state", status.SubState)
		result.Unhealthy = append(result.Unhealthy, unit)
		c.maybeRestart(ctx, unit, result)
	}
}

func (c *Checker) maybeRestart(ctx context.Context, unit string, result *CheckResult) {
	if last, ok := c.lastRestart[unit]; ok {
		elapsed := time.Since(last)
		if elapsed < restartCooldown {
			slog.Info("skipping restart, cooldown active", "unit", unit, "cooldown_remaining", (restartCooldown - elapsed).Round(time.Second))
			result.Skipped = append(result.Skipped, unit)
			return
		}
	}
	if err := c.systemd.RestartUnit(ctx, unit); err != nil {
		slog.Error("restart failed", "unit", unit, "error", err)
		result.Errors = append(result.Errors, err)
		return
	}
	c.lastRestart[unit] = time.Now()
	result.Restarted = append(result.Restarted, unit)
}
