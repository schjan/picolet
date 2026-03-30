package metrics

import (
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// unitStatus is the internal collector map entry.
type unitStatus struct {
	activeState string
	subState    string
}

// UnitHealthCollector emits picolet_unit_active and picolet_unit_state_info for
// all managed units on each Prometheus scrape. Stale series are impossible by
// construction — Collect() only emits what is currently in the map.
type UnitHealthCollector struct {
	mu    sync.RWMutex
	units map[string]unitStatus

	descActive *prometheus.Desc
	descInfo   *prometheus.Desc
}

// UnitHealth is the package-level collector instance registered at startup.
var UnitHealth = NewUnitHealthCollector()

// NewUnitHealthCollector creates a new UnitHealthCollector. Exported for testing.
func NewUnitHealthCollector() *UnitHealthCollector {
	return &UnitHealthCollector{
		units: make(map[string]unitStatus),
		descActive: prometheus.NewDesc(
			"picolet_unit_active",
			"1 if the managed unit is active, 0 if failed. Absent for inactive/oneshot units.",
			[]string{"unit"}, nil,
		),
		descInfo: prometheus.NewDesc(
			"picolet_unit_state_info",
			"Info metric (value=1) for managed unit status. Join with picolet_unit_active via group_left.",
			[]string{"unit", "active_state", "sub_state"}, nil,
		),
	}
}

// Describe implements prometheus.Collector. Always emits both descriptors
// unconditionally — required by the Prometheus collector contract.
func (c *UnitHealthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descActive
	ch <- c.descInfo
}

// Collect implements prometheus.Collector.
func (c *UnitHealthCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for unit, s := range c.units {
		var activeVal float64
		switch s.activeState {
		case "active", "activating":
			activeVal = 1
		case "failed":
			activeVal = 0
		default:
			// "inactive", "deactivating", "reloading", "maintenance": expected/transitional.
			// Absent from metrics — prevents false alerts for timer/oneshot services.
			continue
		}
		mActive, err := prometheus.NewConstMetric(c.descActive, prometheus.GaugeValue, activeVal, unit)
		if err != nil {
			slog.Debug("skipping unit active metric", "unit", unit, "error", err)
			continue
		}
		mInfo, err := prometheus.NewConstMetric(c.descInfo, prometheus.GaugeValue, 1, unit, s.activeState, s.subState)
		if err != nil {
			slog.Debug("skipping unit info metric", "unit", unit, "error", err)
			continue
		}
		ch <- mActive
		ch <- mInfo
	}
}

// Set updates the status for a unit. Called from the health check loop each tick.
func (c *UnitHealthCollector) Set(unit, activeState, subState string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.units[unit] = unitStatus{activeState: activeState, subState: subState}
}

// Delete removes a unit from the collector. Called when a unit leaves management.
func (c *UnitHealthCollector) Delete(unit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.units, unit)
}

// Clear removes all units from the collector. Called when D-Bus is fully down
// so stale gauges disappear from scrapes (absent, not 0).
func (c *UnitHealthCollector) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.units)
}
