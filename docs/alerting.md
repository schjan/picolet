# Alerting & Metrics Reference

## Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `picolet_reconciliation_total` | Counter | `result` (success/failure/noop) | Total reconciliation attempts |
| `picolet_reconciliation_duration_seconds` | Histogram | — | Duration of reconciliation cycles |
| `picolet_last_successful_reconciliation_timestamp` | Gauge | — | Unix timestamp of last successful reconciliation |
| `picolet_git_poll_total` | Counter | `result` (changed/noop/error) | Total git poll attempts |
| `picolet_files_applied_total` | Counter | `action`, `category` | Files applied per action (create/update/delete) and category |
| `picolet_files_managed_total` | Gauge | `category` | Current managed files by category |
| `picolet_managed_units_total` | Gauge | — | Total number of managed units |
| `picolet_failed_sha_consecutive_count` | Gauge | — | Consecutive failures for current SHA (gates at 3) |
| `picolet_drift_detected_total` | Counter | — | Total drift detections (unhealthy units) |
| `picolet_rollback_total` | Counter | — | Total rollbacks performed |
| `picolet_health_check_total` | Counter | `unit`, `result` | Health checks by unit and result |
| `picolet_health_enforcement_total` | Counter | `unit`, `action` | Health enforcement actions by unit |
| `picolet_applied_git_sha_info` | Gauge | `sha` | Currently applied git SHA (value=1) |
| `picolet_self_update_pending` | Gauge | — | 1 when picolet.container changed but not yet restarted |
| `picolet_orphans_removed_total` | Counter | `type` (file/secret) | Orphaned resources removed at startup |

## Recommended Alert Rules

```yaml
groups:
  - name: picolet
    rules:
      - alert: PicoletReconciliationStale
        expr: time() - picolet_last_successful_reconciliation_timestamp > 600
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "No successful reconciliation in 10 minutes"
          description: "picolet on {{ $labels.instance }} has not reconciled successfully for over 10 minutes."

      - alert: PicoletSHAPermanentlyFailed
        expr: picolet_failed_sha_consecutive_count >= 3
        labels:
          severity: critical
        annotations:
          summary: "picolet has gated a SHA after 3 consecutive failures"
          description: "The current git HEAD has failed reconciliation 3+ times and is permanently skipped until a new commit arrives."

      - alert: PicoletSelfUpdateStuck
        expr: picolet_self_update_pending > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Self-update pending for >10m, systemd may have rejected restart"
          description: "picolet.container was updated but the agent has not restarted. Check systemd journal for restart failures."
```
