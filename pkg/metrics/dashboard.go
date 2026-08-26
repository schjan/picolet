package metrics

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

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
			[]string{"role"}, nil,
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
	if host.Role != "" {
		ch <- prometheus.MustNewConstMetric(c.descHost, prometheus.GaugeValue, 1, host.Role)
	}
	for _, feature := range host.Features {
		ch <- prometheus.MustNewConstMetric(c.descFeature, prometheus.GaugeValue, 1, feature)
	}
}
