package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

// LastImagePruneCollector emits picolet_last_image_prune_timestamp from the
// injected *status.Store. The series is omitted until the first prune has run
// (LastRunAt is zero), so "age since last prune" queries are never poisoned by
// the Unix epoch and absent() distinguishes "never pruned" from "overdue".
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
