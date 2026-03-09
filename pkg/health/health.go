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
	Errors    []error
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

// Enforce checks all managed units and restarts any that are not active,
// subject to a per-unit restart cooldown.
func (c *Checker) Enforce(ctx context.Context, st *state.State) (*CheckResult, error) {
	result := &CheckResult{}

	// Derive unique unit names from the service names map
	units := make(map[string]bool)
	for _, unitName := range st.ServiceNames {
		if unitName != "" {
			units[unitName] = true
		}
	}

	for unit := range units {
		active, err := c.systemd.IsActive(ctx, unit)
		if err != nil {
			slog.Warn("health check failed", "unit", unit, "error", err)
			result.Errors = append(result.Errors, err)
			continue
		}

		if active {
			result.Healthy = append(result.Healthy, unit)
			continue
		}

		slog.Warn("unit not active, restarting", "unit", unit)
		result.Unhealthy = append(result.Unhealthy, unit)

		if last, ok := c.lastRestart[unit]; ok && time.Since(last) < restartCooldown {
			slog.Warn("skipping restart, cooldown active", "unit", unit, "cooldown_remaining", (restartCooldown - time.Since(last)).Round(time.Second))
			continue
		}

		if err := c.systemd.RestartUnit(ctx, unit); err != nil {
			slog.Error("restart failed", "unit", unit, "error", err)
			result.Errors = append(result.Errors, err)
			continue
		}
		c.lastRestart[unit] = time.Now()
	}

	return result, nil
}
