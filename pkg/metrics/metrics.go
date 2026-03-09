package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ReconciliationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_reconciliation_total",
			Help: "Total number of reconciliation attempts by result.",
		},
		[]string{"result"},
	)

	ReconciliationDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "picolet_reconciliation_duration_seconds",
			Help:    "Duration of reconciliation cycles.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
		},
	)

	LastSuccessfulReconciliation = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_last_successful_reconciliation_timestamp",
			Help: "Unix timestamp of the last successful reconciliation.",
		},
	)

	RollbackTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "picolet_rollback_total",
			Help: "Total number of rollbacks performed.",
		},
	)

	HealthCheckTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_health_check_total",
			Help: "Total health checks by unit and result.",
		},
		[]string{"unit", "result"},
	)

	HealthEnforcementTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_health_enforcement_total",
			Help: "Total health enforcement actions by unit and action.",
		},
		[]string{"unit", "action"},
	)

	AppliedGitSHA = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_applied_git_sha_info",
			Help: "Currently applied git SHA (value=1).",
		},
		[]string{"sha"},
	)

	ManagedFilesTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_managed_files_total",
			Help: "Total number of managed files.",
		},
	)

	GitPollTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_git_poll_total",
			Help: "Total git poll attempts by result.",
		},
		[]string{"result"},
	)

	FilesAppliedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_files_applied_total",
			Help: "Total files applied by action and category.",
		},
		[]string{"action", "category"},
	)

	FilesManagedTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_files_managed_total",
			Help: "Current number of managed files by category.",
		},
		[]string{"category"},
	)

	FailedSHAConsecutiveCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_failed_sha_consecutive_count",
			Help: "Consecutive reconciliation failures for the current SHA.",
		},
	)

	OrphansRemovedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_orphans_removed_total",
			Help: "Orphaned files/secrets removed at startup by type.",
		},
		[]string{"type"},
	)
)

var registerOnce sync.Once

// Register registers all metrics with the default Prometheus registry.
// Safe to call multiple times (e.g. in tests).
func Register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			ReconciliationTotal,
			ReconciliationDuration,
			LastSuccessfulReconciliation,
			RollbackTotal,
			HealthCheckTotal,
			HealthEnforcementTotal,
			AppliedGitSHA,
			ManagedFilesTotal,
			GitPollTotal,
			FilesAppliedTotal,
			FilesManagedTotal,
			FailedSHAConsecutiveCount,
			OrphansRemovedTotal,
		)
	})
}

// Handler returns an http.Handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}
