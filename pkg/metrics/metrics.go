package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/schjan/picolet/pkg/status"
	"github.com/schjan/picolet/pkg/version"
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

	MQTTConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "picolet_mqtt_connected",
		Help: "1 if the agent has an active MQTT broker connection, 0 otherwise.",
	})

	AgentPaused = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "picolet_agent_paused",
		Help: "1 if the agent is paused via MQTT, 0 otherwise.",
	})

	DBusConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "picolet_dbus_connected",
		Help: "1 if the D-Bus connection to systemd is alive, 0 otherwise.",
	})

	HealthCheckErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "picolet_health_check_errors_total",
		Help: "Total health check errors (D-Bus failures, not per-unit).",
	})

	DeploymentStatusTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_deployment_status_total",
			Help: "Total deployment status reports by result.",
		},
		[]string{"result"},
	)

	BuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_build_info",
			Help: "Picolet build information (always 1).",
		},
		[]string{"version", "git_sha"},
	)

	FeatureInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_feature_info",
			Help: "Configured optional features (1=enabled, 0=disabled).",
		},
		[]string{"feature"},
	)

	SecretsManagedCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_secrets_managed_count",
			Help: "Number of direct provider-backed secret assignments currently managed, per provider.",
		},
		[]string{"provider"},
	)

	SecretSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_secret_sync_total",
			Help: "Total secret-provider sync attempts by provider and result.",
		},
		[]string{"provider", "result"},
	)

	SecretLastSyncTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_secret_last_sync_timestamp",
			Help: "Unix timestamp of the last successful secret-provider sync, per provider.",
		},
		[]string{"provider"},
	)

	SecretCredentialExpiresAt = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "picolet_secret_credential_expires_at",
			Help: "Unix timestamp at which the configured credential (service-account token, PAT) expires, per provider. Only emitted when the operator records the expiry in config.",
		},
		[]string{"provider"},
	)

	HookTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "picolet_hook_total",
			Help: "Total hook executions by name, action, and result.",
		},
		[]string{"name", "action", "result"},
	)
)

var registerOnce sync.Once

// Register registers all metrics with the default Prometheus registry.
// Custom collectors that read runtime state are constructed against the
// provided *status.Store. Safe to call multiple times (e.g. in tests).
func Register(store *status.Store) {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			ReconciliationTotal,
			ReconciliationDuration,
			LastSuccessfulReconciliation,
			RollbackTotal,
			HealthCheckTotal,
			HealthEnforcementTotal,
			appliedSHA,
			GitPollTotal,
			FilesAppliedTotal,
			FilesManagedTotal,
			FailedSHAConsecutiveCount,
			OrphansRemovedTotal,
			MQTTConnected,
			AgentPaused,
			DBusConnected,
			HealthCheckErrorsTotal,
			DeploymentStatusTotal,
			BuildInfo,
			FeatureInfo,
			SecretsManagedCount,
			SecretSyncTotal,
			SecretLastSyncTimestamp,
			SecretCredentialExpiresAt,
			HookTotal,
			NewUnitHealthCollector(store),
			NewUnitDependencyCollector(store),
			NewHostInfoCollector(store),
		)
		BuildInfo.WithLabelValues(version.Version, version.GitSHA).Set(1)
	})
}

// Handler returns an http.Handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}

// appliedSHACollector implements prometheus.Collector for the applied git SHA info metric.
// On each scrape it emits a single gauge with value=1 for the current SHA.
// Stale label series are impossible — only the current SHA is ever emitted.
type appliedSHACollector struct {
	desc *prometheus.Desc
	mu   sync.RWMutex
	sha  string
}

var appliedSHA = &appliedSHACollector{
	desc: prometheus.NewDesc(
		"picolet_applied_git_sha_info",
		"Currently applied git SHA (value=1).",
		[]string{"sha"}, nil,
	),
}

func (c *appliedSHACollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *appliedSHACollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	sha := c.sha
	c.mu.RUnlock()
	if sha == "" {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1, sha)
}

// SetAppliedSHA updates the currently applied git SHA exposed to Prometheus.
// Use this for seeding from persisted state. For recording a successful apply,
// use RecordAppliedSHA which also resets failure count and updates the timestamp.
func SetAppliedSHA(sha string) {
	appliedSHA.mu.Lock()
	appliedSHA.sha = sha
	appliedSHA.mu.Unlock()
}

// RecordAppliedSHA records a successful SHA application: sets the applied SHA,
// resets the consecutive failure count, and updates the last successful timestamp.
func RecordAppliedSHA(sha string) {
	SetAppliedSHA(sha)
	FailedSHAConsecutiveCount.Set(0)
	LastSuccessfulReconciliation.SetToCurrentTime()
}

// RecordFailedSHA records a reconciliation failure for the given consecutive count.
func RecordFailedSHA(consecutiveCount int) {
	FailedSHAConsecutiveCount.Set(float64(consecutiveCount))
}
