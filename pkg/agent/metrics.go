package agent

import (
	"log/slog"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

func (a *Agent) recordHealthMetrics(hr *health.CheckResult) {
	for _, u := range hr.Healthy {
		metrics.HealthCheckTotal.WithLabelValues(u, "healthy").Inc()
	}
	for _, u := range hr.Unhealthy {
		metrics.HealthCheckTotal.WithLabelValues(u, "unhealthy").Inc()
	}
	for _, u := range hr.Inactive {
		metrics.HealthCheckTotal.WithLabelValues(u, "inactive").Inc()
	}
	for _, u := range hr.Restarted {
		metrics.HealthEnforcementTotal.WithLabelValues(u, "restart").Inc()
	}
	for _, u := range hr.Skipped {
		metrics.HealthEnforcementTotal.WithLabelValues(u, "skip_cooldown").Inc()
	}
	// Status store is the single source of truth for per-unit health.
	// metrics.NewUnitHealthCollector reads its scrape data from the store.
	a.statusStore.SetUnits(unitStatusesFromHealth(hr.Statuses))

	metrics.HealthCheckErrorsTotal.Add(float64(len(hr.Errors)))

	if hr.AllFailed() {
		a.statusStore.ClearUnits()
	}
}

func unitStatusesFromHealth(statuses map[string]applier.UnitStatus) map[string]status.UnitRuntimeStatus {
	out := make(map[string]status.UnitRuntimeStatus, len(statuses))
	for unit, st := range statuses {
		out[unit] = status.UnitRuntimeStatus{ActiveState: st.ActiveState, SubState: st.SubState}
	}
	return out
}

func countCategoriesFromState(managed map[string]state.ManagedFile) map[string]float64 {
	counts := make(map[string]float64, len(reconciler.Categories()))
	for _, mf := range managed {
		counts[mf.Category.String()]++
	}
	return counts
}

// setFilesManagedMetric overwrites FilesManagedTotal for every known category.
// Because the label set is fixed, each call is a pure Set — no Reset() needed,
// so a concurrent Prometheus scrape never sees zero or partial values.
func setFilesManagedMetric(counts map[string]float64) {
	for _, cat := range reconciler.Categories() {
		category := cat.String()
		metrics.FilesManagedTotal.WithLabelValues(category).Set(counts[category])
	}
}

// recordProviderRefCount publishes the per-provider managed-count gauge
// and returns the count. Returns 0 when the provider is disabled.
func recordProviderRefCount(provider resolver.ProviderKey, enabled bool, isRef func(string) bool, files []resolver.ResolvedFile) int {
	if !enabled {
		return 0
	}
	count := countRefs(files, isRef)
	metrics.SecretsManagedCount.WithLabelValues(string(provider)).Set(float64(count))
	return count
}

// recordProviderSyncSuccess bumps the success counter and last-sync gauge
// for a provider after a tick that actually resolved at least one ref.
// A zero count means "nothing to record" — disabled providers always produce
// zero counts via recordProviderRefCount, so a single count==0 check covers
// both cases.
func recordProviderSyncSuccess(provider resolver.ProviderKey, count int) {
	if count == 0 {
		return
	}
	label := string(provider)
	metrics.SecretSyncTotal.WithLabelValues(label).Inc()
	metrics.SecretLastSyncTimestamp.WithLabelValues(label).SetToCurrentTime()
}

// markRefreshAttempted bumps every configured provider's last-refresh
// timestamp so the agent does not loop tight on the refresh trigger while
// a tick is gated, partial, or has just completed successfully.
func (a *Agent) markRefreshAttempted() {
	now := time.Now()
	if a.opReader != nil {
		a.lastOPRefresh = now
	}
	if a.ppReader != nil {
		a.lastPPRefresh = now
	}
}

func countRefs(files []resolver.ResolvedFile, isRef func(string) bool) int {
	var count int
	for _, f := range files {
		if isRef(f.SrcPath) {
			count++
		}
	}
	return count
}

// publishCredentialExpiry sets the picolet_secret_credential_expires_at gauge
// for the given provider when the operator has recorded the expiration in
// config. Zero values are skipped (no series emitted), so dashboards can use
// `absent_over_time(...)` to flag providers whose expiry was never declared.
// Past expirations are still published — that is the signal the alert needs.
func publishCredentialExpiry(provider resolver.ProviderKey, expiresAt time.Time) {
	if expiresAt.IsZero() {
		return
	}
	metrics.SecretCredentialExpiresAt.WithLabelValues(string(provider)).Set(float64(expiresAt.Unix()))
	if time.Now().After(expiresAt) {
		slog.Warn("secret-provider credential has expired", "provider", provider, "expired_at", expiresAt.Format(time.RFC3339))
	}
}

func (a *Agent) recordHostMetadata(host *config.HostConfig) {
	if host == nil {
		return
	}
	a.statusStore.SetHost(status.HostMetadata{
		PiType:           host.PiType,
		Features:         host.Features,
		ExternalHostname: host.ExternalHostname,
	})
}

func recordHookMetrics(result *applier.ApplyResult) {
	if result == nil {
		return
	}
	for _, o := range result.HookOutcomes {
		metrics.HookTotal.WithLabelValues(o.Name, o.Action, o.Result).Inc()
	}
}
