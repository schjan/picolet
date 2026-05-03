package metrics

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

// UnitHealthCollector emits picolet_unit_active and picolet_unit_state_info for
// all managed units on each Prometheus scrape, reading from the injected
// *status.Store. Stale series are impossible by construction — Collect emits
// only what the store currently holds.
type UnitHealthCollector struct {
	store      *status.Store
	descActive *prometheus.Desc
	descInfo   *prometheus.Desc
}

// NewUnitHealthCollector constructs a UnitHealthCollector reading from store.
// The store is the single source of truth for unit runtime status.
func NewUnitHealthCollector(store *status.Store) *UnitHealthCollector {
	return &UnitHealthCollector{
		store: store,
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

// Describe implements prometheus.Collector.
func (c *UnitHealthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descActive
	ch <- c.descInfo
}

// Collect implements prometheus.Collector.
func (c *UnitHealthCollector) Collect(ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	for unit, s := range c.store.Snapshot().Units {
		var activeVal float64
		switch s.ActiveState {
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
		mInfo, err := prometheus.NewConstMetric(c.descInfo, prometheus.GaugeValue, 1, unit, s.ActiveState, s.SubState)
		if err != nil {
			slog.Debug("skipping unit info metric", "unit", unit, "error", err)
			continue
		}
		ch <- mActive
		ch <- mInfo
	}
}
