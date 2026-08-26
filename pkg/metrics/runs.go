package metrics

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

// UnitRunCollector emits the run bookkeeping of timer-triggered one-shots —
// "when did this scheduled job last run, with what result, and when did it last
// succeed" — plus the last-trigger time of the managed .timers that fire them.
//
// Every series is read from the injected *status.Store on each scrape, so
// cardinality is bounded by the Managed units of the Fleet and stale series are
// impossible: a unit that leaves the Fleet loses its record, and nothing else
// does. A value systemd has never observed is absent rather than zero, so
// `absent()` distinguishes "never ran" from "overdue" and `time() - series`
// queries are never poisoned by the Unix epoch.
type UnitRunCollector struct {
	store           *status.Store
	descLastSuccess *prometheus.Desc
	descLastRun     *prometheus.Desc
	descLastResult  *prometheus.Desc
	descLastTrigger *prometheus.Desc
}

// NewUnitRunCollector constructs a UnitRunCollector reading from store.
func NewUnitRunCollector(store *status.Store) *UnitRunCollector {
	return &UnitRunCollector{
		store: store,
		descLastSuccess: prometheus.NewDesc(
			"picolet_unit_last_success_timestamp_seconds",
			"Unix timestamp at which a timer-triggered one-shot last completed successfully. Absent until the first success.",
			[]string{"unit"}, nil,
		),
		descLastRun: prometheus.NewDesc(
			"picolet_unit_last_run_timestamp_seconds",
			"Unix timestamp at which a timer-triggered one-shot last started, whatever the outcome. Absent until the first run.",
			[]string{"unit"}, nil,
		),
		descLastResult: prometheus.NewDesc(
			"picolet_unit_last_result",
			"Info metric (value=1) for a timer-triggered one-shot's current systemd Result= "+
				"(success, exit-code, timeout, signal, ...). systemd resets it to success when a run starts, "+
				"so join with picolet_unit_last_success_timestamp_seconds when the last completed outcome matters. "+
				"Absent until the unit has run.",
			[]string{"unit", "result"}, nil,
		),
		descLastTrigger: prometheus.NewDesc(
			"picolet_timer_last_trigger_timestamp_seconds",
			"Unix timestamp at which a managed .timer last fired. Absent until the first trigger.",
			[]string{"unit"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *UnitRunCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descLastSuccess
	ch <- c.descLastRun
	ch <- c.descLastResult
	ch <- c.descLastTrigger
}

// Collect implements prometheus.Collector.
func (c *UnitRunCollector) Collect(ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	for unit, run := range c.store.Snapshot().Runs {
		if !run.StartedAt.IsZero() {
			emitRunMetric(ch, c.descLastRun, float64(run.StartedAt.Unix()), unit)
		}
		if !run.SucceededAt.IsZero() {
			emitRunMetric(ch, c.descLastSuccess, float64(run.SucceededAt.Unix()), unit)
		}
		if !run.TriggeredAt.IsZero() {
			emitRunMetric(ch, c.descLastTrigger, float64(run.TriggeredAt.Unix()), unit)
		}
		if run.Result != "" {
			emitRunMetric(ch, c.descLastResult, 1, unit, run.Result)
		}
	}
}

// emitRunMetric sends one gauge sample, logging and dropping a label-cardinality
// mismatch rather than panicking the scrape.
func emitRunMetric(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	if err != nil {
		slog.Debug("skipping unit run metric", "desc", desc.String(), "labels", labels, "error", err)
		return
	}
	ch <- m
}
