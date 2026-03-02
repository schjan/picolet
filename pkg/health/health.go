package health

import (
	"context"
	"log/slog"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

// CheckResult holds the outcome of a health enforcement pass.
type CheckResult struct {
	Healthy   []string
	Unhealthy []string
	Errors    []error
}

// Checker enforces that managed systemd units are active.
type Checker struct {
	systemd applier.SystemdManager
}

// New creates a new health Checker.
func New(systemd applier.SystemdManager) *Checker {
	return &Checker{systemd: systemd}
}

// Enforce checks all managed units and restarts any that are not active.
func (c *Checker) Enforce(ctx context.Context, st *state.State) (*CheckResult, error) {
	result := &CheckResult{}

	// Derive unique unit names from managed files
	units := make(map[string]bool)
	for destPath := range st.ManagedFiles {
		unitName := validator.UnitNameFromPath(destPath)
		if unitName == "" {
			continue
		}
		units[unitName] = true
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

		if err := c.systemd.RestartUnit(ctx, unit); err != nil {
			slog.Error("restart failed", "unit", unit, "error", err)
			result.Errors = append(result.Errors, err)
		}
	}

	return result, nil
}
