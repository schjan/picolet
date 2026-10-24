package metrics_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/status"
)

// productionDescs is the /metrics surface (name, help, labels) as it stood
// before the secret-provider and failed-SHA series moved behind the status
// store. Changing a line here changes what dashboards and alerts see.
var productionDescs = []string{
	`Desc{fqName: "picolet_agent_paused", help: "1 if the agent is paused via MQTT, 0 otherwise.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_applied_git_sha_info", help: "Currently applied git SHA (value=1).", unit: "", constLabels: {}, variableLabels: {sha}}`,
	`Desc{fqName: "picolet_build_info", help: "Picolet build information (always 1).", unit: "", constLabels: {}, variableLabels: {version,git_sha}}`,
	`Desc{fqName: "picolet_dbus_connected", help: "1 if the D-Bus connection to systemd is alive, 0 otherwise.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_deployment_status_total", help: "Total deployment status reports by result.", unit: "", constLabels: {}, variableLabels: {result}}`,
	`Desc{fqName: "picolet_failed_sha_consecutive_count", help: "Consecutive reconciliation failures for the current SHA.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_feature_info", help: "Configured optional features (1=enabled, 0=disabled).", unit: "", constLabels: {}, variableLabels: {feature}}`,
	`Desc{fqName: "picolet_files_applied_total", help: "Total files applied by action and category.", unit: "", constLabels: {}, variableLabels: {action,category}}`,
	`Desc{fqName: "picolet_files_managed_total", help: "Current number of managed files by category.", unit: "", constLabels: {}, variableLabels: {category}}`,
	`Desc{fqName: "picolet_git_poll_total", help: "Total git poll attempts by result.", unit: "", constLabels: {}, variableLabels: {result}}`,
	`Desc{fqName: "picolet_health_check_errors_total", help: "Total health check errors (D-Bus failures, not per-unit).", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_health_check_total", help: "Total health checks by unit and result.", unit: "", constLabels: {}, variableLabels: {unit,result}}`,
	`Desc{fqName: "picolet_health_enforcement_total", help: "Total health enforcement actions by unit and action.", unit: "", constLabels: {}, variableLabels: {unit,action}}`,
	`Desc{fqName: "picolet_hook_total", help: "Total hook executions by name, action, and result.", unit: "", constLabels: {}, variableLabels: {name,action,result}}`,
	`Desc{fqName: "picolet_host_feature_info", help: "Resolved host feature metadata (value=1).", unit: "", constLabels: {}, variableLabels: {feature}}`,
	`Desc{fqName: "picolet_host_info", help: "Resolved host metadata (value=1).", unit: "", constLabels: {}, variableLabels: {role,machine,user}}`,
	`Desc{fqName: "picolet_image_prune_reclaimed_bytes_total", help: "Total bytes reclaimed by scheduled image pruning.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_image_prune_total", help: "Total scheduled image-prune runs by result.", unit: "", constLabels: {}, variableLabels: {result}}`,
	`Desc{fqName: "picolet_images_pruned_total", help: "Total number of unused images removed by scheduled pruning.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_last_image_prune_timestamp", help: "Unix timestamp of the last successful image prune. Absent until the first prune.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_last_successful_reconciliation_timestamp", help: "Unix timestamp of the last successful reconciliation.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_mqtt_connected", help: "1 if the agent has an active MQTT broker connection, 0 otherwise.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_orphans_removed_total", help: "Orphaned files/secrets removed at startup by type.", unit: "", constLabels: {}, variableLabels: {type}}`,
	`Desc{fqName: "picolet_reconciliation_duration_seconds", help: "Duration of reconciliation cycles.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_reconciliation_total", help: "Total number of reconciliation attempts by result.", unit: "", constLabels: {}, variableLabels: {result}}`,
	`Desc{fqName: "picolet_rollback_total", help: "Total number of rollbacks performed.", unit: "", constLabels: {}, variableLabels: {}}`,
	`Desc{fqName: "picolet_secret_credential_expires_at", help: "Unix timestamp at which the configured credential (service-account token, PAT) expires, per provider. Only emitted when the operator records the expiry in config.", unit: "", constLabels: {}, variableLabels: {provider}}`,
	`Desc{fqName: "picolet_secret_last_sync_timestamp", help: "Unix timestamp of the last successful secret-provider sync, per provider.", unit: "", constLabels: {}, variableLabels: {provider}}`,
	`Desc{fqName: "picolet_secret_sync_total", help: "Successful secret-provider sync attempts, per provider. Failures are counted on picolet_reconciliation_total{result=\"failure\"}.", unit: "", constLabels: {}, variableLabels: {provider}}`,
	`Desc{fqName: "picolet_secrets_managed_count", help: "Number of direct provider-backed secret assignments currently managed, per provider.", unit: "", constLabels: {}, variableLabels: {provider}}`,
	`Desc{fqName: "picolet_systemd_unit_operations_total", help: "Total enable/disable/start/restart operations on raw systemd units (result=skipped: a timer-triggered one-shot restart was declined), by operation and result.", unit: "", constLabels: {}, variableLabels: {operation,result}}`,
	`Desc{fqName: "picolet_timer_last_trigger_timestamp_seconds", help: "Unix timestamp at which a managed .timer last fired. Absent until the first trigger.", unit: "", constLabels: {}, variableLabels: {unit}}`,
	`Desc{fqName: "picolet_unit_active", help: "1 if the managed unit is active, 0 if failed. Absent for inactive/oneshot units.", unit: "", constLabels: {}, variableLabels: {unit}}`,
	`Desc{fqName: "picolet_unit_dependency_count", help: "Number of generated systemd dependencies for a managed unit by relation.", unit: "", constLabels: {}, variableLabels: {unit,relation}}`,
	`Desc{fqName: "picolet_unit_last_result", help: "Info metric (value=1) for a timer-triggered one-shot's current systemd Result= (success, exit-code, timeout, signal, ...). systemd resets it to success when a run starts, so join with picolet_unit_last_success_timestamp_seconds when the last completed outcome matters. Absent until the unit has run.", unit: "", constLabels: {}, variableLabels: {unit,result}}`,
	`Desc{fqName: "picolet_unit_last_run_timestamp_seconds", help: "Unix timestamp at which a timer-triggered one-shot last started, whatever the outcome. Absent until the first run.", unit: "", constLabels: {}, variableLabels: {unit}}`,
	`Desc{fqName: "picolet_unit_last_success_timestamp_seconds", help: "Unix timestamp at which a timer-triggered one-shot last completed successfully. Absent until the first success.", unit: "", constLabels: {}, variableLabels: {unit}}`,
	`Desc{fqName: "picolet_unit_restart_pending", help: "Managed units whose last restart attempt failed (value = consecutive failed attempts).", unit: "", constLabels: {}, variableLabels: {unit}}`,
	`Desc{fqName: "picolet_unit_state_info", help: "Info metric (value=1) for managed unit status. Join with picolet_unit_active via group_left.", unit: "", constLabels: {}, variableLabels: {unit,active_state,sub_state}}`,
}

func TestCollectors_ProductionSurfaceUnchanged(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.Collectors(status.NewStore())...)

	ch := make(chan *prometheus.Desc, 2*len(productionDescs))
	go func() {
		reg.Describe(ch)
		close(ch)
	}()
	var got []string
	for d := range ch {
		got = append(got, d.String())
	}
	slices.Sort(got)
	assert.Equal(t, productionDescs, got)
}

func TestCollectors_SecretAndGateSeriesFromStore(t *testing.T) {
	t.Parallel()
	store := status.NewStore()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.Collectors(store)...)

	store.SetSecretsManaged("onepassword", 3)
	store.SetSecretsManaged("protonpass", 0)
	store.RecordSecretSync("onepassword", time.Unix(1700000000, 0))
	store.RecordSecretSync("onepassword", time.Unix(1700000100, 0))
	store.SetCredentialExpiry("onepassword", time.Unix(1900000000, 0))
	store.SetFailedSHACount(2)

	expected := `
# HELP picolet_failed_sha_consecutive_count Consecutive reconciliation failures for the current SHA.
# TYPE picolet_failed_sha_consecutive_count gauge
picolet_failed_sha_consecutive_count 2
# HELP picolet_secret_credential_expires_at Unix timestamp at which the configured credential (service-account token, PAT) expires, per provider. Only emitted when the operator records the expiry in config.
# TYPE picolet_secret_credential_expires_at gauge
picolet_secret_credential_expires_at{provider="onepassword"} 1.9e+09
# HELP picolet_secret_last_sync_timestamp Unix timestamp of the last successful secret-provider sync, per provider.
# TYPE picolet_secret_last_sync_timestamp gauge
picolet_secret_last_sync_timestamp{provider="onepassword"} 1.7000001e+09
# HELP picolet_secret_sync_total Successful secret-provider sync attempts, per provider. Failures are counted on picolet_reconciliation_total{result="failure"}.
# TYPE picolet_secret_sync_total counter
picolet_secret_sync_total{provider="onepassword"} 2
# HELP picolet_secrets_managed_count Number of direct provider-backed secret assignments currently managed, per provider.
# TYPE picolet_secrets_managed_count gauge
picolet_secrets_managed_count{provider="onepassword"} 3
picolet_secrets_managed_count{provider="protonpass"} 0
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"picolet_failed_sha_consecutive_count",
		"picolet_secret_credential_expires_at",
		"picolet_secret_last_sync_timestamp",
		"picolet_secret_sync_total",
		"picolet_secrets_managed_count",
	))
}

// Before anything is recorded the failed-SHA gauge reads 0 (as the plain gauge
// did) and the per-provider series are absent.
func TestCollectors_EmptyStore(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.Collectors(status.NewStore())...)

	expected := `
# HELP picolet_failed_sha_consecutive_count Consecutive reconciliation failures for the current SHA.
# TYPE picolet_failed_sha_consecutive_count gauge
picolet_failed_sha_consecutive_count 0
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"picolet_failed_sha_consecutive_count",
		"picolet_secret_credential_expires_at",
		"picolet_secret_last_sync_timestamp",
		"picolet_secret_sync_total",
		"picolet_secrets_managed_count",
	))
}
