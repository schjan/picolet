package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/schjan/picolet/pkg/status"
)

// SecretProviderCollector emits the per-provider secret-sync series from the
// injected *status.Store, so each Agent's (and each test's) store is scraped
// on its own.
//   - picolet_secrets_managed_count once a managed count was recorded.
//   - picolet_secret_sync_total and picolet_secret_last_sync_timestamp once the
//     provider synced successfully.
//   - picolet_secret_credential_expires_at once an expiry was declared.
type SecretProviderCollector struct {
	store       *status.Store
	descManaged *prometheus.Desc
	descSyncs   *prometheus.Desc
	descLast    *prometheus.Desc
	descExpires *prometheus.Desc
}

// NewSecretProviderCollector constructs the collector reading from store.
func NewSecretProviderCollector(store *status.Store) *SecretProviderCollector {
	labels := []string{"provider"}
	return &SecretProviderCollector{
		store: store,
		descManaged: prometheus.NewDesc(
			"picolet_secrets_managed_count",
			"Number of direct provider-backed secret assignments currently managed, per provider.",
			labels, nil,
		),
		descSyncs: prometheus.NewDesc(
			"picolet_secret_sync_total",
			"Successful secret-provider sync attempts, per provider. Failures are counted on picolet_reconciliation_total{result=\"failure\"}.",
			labels, nil,
		),
		descLast: prometheus.NewDesc(
			"picolet_secret_last_sync_timestamp",
			"Unix timestamp of the last successful secret-provider sync, per provider.",
			labels, nil,
		),
		descExpires: prometheus.NewDesc(
			"picolet_secret_credential_expires_at",
			"Unix timestamp at which the configured credential (service-account token, PAT) expires, per provider. Only emitted when the operator records the expiry in config.",
			labels, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *SecretProviderCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descManaged
	ch <- c.descSyncs
	ch <- c.descLast
	ch <- c.descExpires
}

// Collect implements prometheus.Collector.
func (c *SecretProviderCollector) Collect(ch chan<- prometheus.Metric) {
	for provider, p := range c.store.SecretProviders() {
		if p.Managed != nil {
			ch <- prometheus.MustNewConstMetric(c.descManaged, prometheus.GaugeValue, float64(*p.Managed), provider)
		}
		if p.Syncs > 0 {
			ch <- prometheus.MustNewConstMetric(c.descSyncs, prometheus.CounterValue, float64(p.Syncs), provider)
			ch <- prometheus.MustNewConstMetric(c.descLast, prometheus.GaugeValue, float64(p.LastSyncAt.UnixNano())/1e9, provider)
		}
		if !p.CredentialExpiresAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.descExpires, prometheus.GaugeValue, float64(p.CredentialExpiresAt.Unix()), provider)
		}
	}
}

// FailedSHACollector emits picolet_failed_sha_consecutive_count from the
// injected *status.Store. Like the plain gauge it replaces, it is always
// emitted and reads 0 until a failure is recorded.
type FailedSHACollector struct {
	store *status.Store
	desc  *prometheus.Desc
}

// NewFailedSHACollector constructs the collector reading from store.
func NewFailedSHACollector(store *status.Store) *FailedSHACollector {
	return &FailedSHACollector{
		store: store,
		desc: prometheus.NewDesc(
			"picolet_failed_sha_consecutive_count",
			"Consecutive reconciliation failures for the current SHA.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *FailedSHACollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *FailedSHACollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(c.store.FailedSHACount()))
}
