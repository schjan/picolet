package metrics

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

// This file holds the custom prometheus.Collector implementations that read
// runtime state from an injected *status.Store on each scrape, instead of
// being set imperatively like the package-level metrics in metrics.go.

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

// UnitDependencyCollector emits dependency counts by unit and relation.
type UnitDependencyCollector struct {
	store *status.Store
	desc  *prometheus.Desc
}

// NewUnitDependencyCollector constructs the collector reading from store.
func NewUnitDependencyCollector(store *status.Store) *UnitDependencyCollector {
	return &UnitDependencyCollector{
		store: store,
		desc: prometheus.NewDesc(
			"picolet_unit_dependency_count",
			"Number of generated systemd dependencies for a managed unit by relation.",
			[]string{"unit", "relation"}, nil,
		),
	}
}

func (c *UnitDependencyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *UnitDependencyCollector) Collect(ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	for unit, deps := range c.store.Snapshot().Dependencies {
		c.emit(ch, unit, "requires", len(deps.Requires))
		c.emit(ch, unit, "wants", len(deps.Wants))
		c.emit(ch, unit, "after", len(deps.After))
		c.emit(ch, unit, "before", len(deps.Before))
		c.emit(ch, unit, "binds_to", len(deps.BindsTo))
		c.emit(ch, unit, "part_of", len(deps.PartOf))
	}
}

// emit writes a single dependency-count series. Zero counts are omitted —
// Prometheus convention is to omit absent series rather than emit zeros.
func (c *UnitDependencyCollector) emit(ch chan<- prometheus.Metric, unit, relation string, count int) {
	if count == 0 {
		return
	}
	m, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, float64(count), unit, relation)
	if err != nil {
		slog.Debug("skipping unit dependency metric", "unit", unit, "relation", relation, "error", err)
		return
	}
	ch <- m
}

// HostInfoCollector emits bounded host metadata as info-style metrics.
type HostInfoCollector struct {
	store       *status.Store
	descHost    *prometheus.Desc
	descFeature *prometheus.Desc
}

// NewHostInfoCollector constructs the collector reading from store.
func NewHostInfoCollector(store *status.Store) *HostInfoCollector {
	return &HostInfoCollector{
		store: store,
		descHost: prometheus.NewDesc(
			"picolet_host_info",
			"Resolved host metadata (value=1).",
			[]string{"pi_type"}, nil,
		),
		descFeature: prometheus.NewDesc(
			"picolet_host_feature_info",
			"Resolved host feature metadata (value=1).",
			[]string{"feature"}, nil,
		),
	}
}

func (c *HostInfoCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descHost
	ch <- c.descFeature
}

func (c *HostInfoCollector) Collect(ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	host := c.store.Snapshot().Host
	if host.PiType != "" {
		ch <- prometheus.MustNewConstMetric(c.descHost, prometheus.GaugeValue, 1, host.PiType)
	}
	for _, feature := range host.Features {
		ch <- prometheus.MustNewConstMetric(c.descFeature, prometheus.GaugeValue, 1, feature)
	}
}

// LastImagePruneCollector emits picolet_last_image_prune_timestamp from the
// injected *status.Store. The series is omitted until a successful prune time is
// known — either from a prune in this process or seeded from persisted state on
// restart (a zero LastRunAt emits nothing) — so "age since last prune" queries
// are never poisoned by the Unix epoch and absent() distinguishes "never pruned"
// from "overdue".
type LastImagePruneCollector struct {
	store *status.Store
	desc  *prometheus.Desc
}

// NewLastImagePruneCollector constructs a LastImagePruneCollector reading from store.
func NewLastImagePruneCollector(store *status.Store) *LastImagePruneCollector {
	return &LastImagePruneCollector{
		store: store,
		desc: prometheus.NewDesc(
			"picolet_last_image_prune_timestamp",
			"Unix timestamp of the last successful image prune. Absent until the first prune.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *LastImagePruneCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *LastImagePruneCollector) Collect(ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	last := c.store.Prune().LastRunAt
	if last.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(last.Unix()))
}
