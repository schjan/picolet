//nolint:revive // 'metrics' is the conventional Prometheus package name
package metrics

import (
	"net/http"

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
			Buckets: prometheus.DefBuckets,
		},
	)

	LastSuccessfulReconciliation = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_last_successful_reconciliation_timestamp",
			Help: "Unix timestamp of the last successful reconciliation.",
		},
	)

	DriftDetectedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "picolet_drift_detected_total",
			Help: "Total number of drift detections.",
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

	SelfUpdatePending = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_self_update_pending",
			Help: "1 when picolet.container changed but not yet restarted.",
		},
	)

	ManagedUnitsTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "picolet_managed_units_total",
			Help: "Total number of managed units.",
		},
	)
)

// Register registers all metrics with the default Prometheus registry.
func Register() {
	prometheus.MustRegister(
		ReconciliationTotal,
		ReconciliationDuration,
		LastSuccessfulReconciliation,
		DriftDetectedTotal,
		RollbackTotal,
		HealthCheckTotal,
		HealthEnforcementTotal,
		AppliedGitSHA,
		SelfUpdatePending,
		ManagedUnitsTotal,
	)
}

// Handler returns an http.Handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}
