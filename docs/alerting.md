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
| `picolet_failed_sha_consecutive_count` | Gauge | — | Consecutive failures for current SHA (gates at 3) |
| `picolet_rollback_total` | Counter | — | Total rollbacks performed |
| `picolet_health_check_total` | Counter | `unit`, `result` | Health checks by unit and result |
| `picolet_health_enforcement_total` | Counter | `unit`, `action` (restart/skip_cooldown) | Health enforcement actions by unit |
| `picolet_applied_git_sha_info` | Gauge | `sha` | Currently applied git SHA (value=1) |
| `picolet_orphans_removed_total` | Counter | `type` (file/secret) | Orphaned resources removed at startup |

> **Self-update monitoring:** use node-exporter's `systemd_unit_start_time_seconds` for `picolet.service` to detect restart failures.

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
```

## Split Rules Across Multiple Files

Use one secret template that aggregates many rule fragments from the repo.

```yaml
# secrets/static_alert_rules.yml.tmpl
groups:{{ concatFiles "rules/static_alert_rules/*.yml" | nindent 2 }}
```

```yaml
# rules/static_alert_rules/instance_alerts.yml
- name: instance_alerts
  rules:
    - alert: InstanceDown
      expr: up == 0
      for: 5m
```

Behavior notes:

- `glob` / `concatFiles` resolve files in lexical order.
- Empty glob matches are validation errors.
- `concatFiles` reads files raw and does not render nested templates, so expressions like `{{ $labels.instance }}` pass through.
- Keep rule fragments unindented and let the template own indentation with `nindent`.
- Picolet validates rendered YAML syntax, but backend-specific semantic checks (`promtool`, `vmalert` tooling, etc.) should run in fleet CI.

## Migration From Monolithic Rules

You can keep the same secret assignment and migrate incrementally:

1. Keep the existing secret file name (for example `secrets/prometheus_rules.yml.tmpl`).
2. Replace monolithic inline groups with `concatFiles`:

```yaml
groups:{{ concatFiles "rules/prometheus/*.yml" | nindent 2 }}
```

3. Move each alert group into its own file under `rules/prometheus/`.
4. Keep backend-specific semantic checks in fleet CI (Prometheus, VictoriaMetrics/vmalert, etc.).
