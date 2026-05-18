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
| `picolet_unit_dependency_count` | Gauge | `unit`, `relation` | Current generated systemd dependency count by managed unit and relation |
| `picolet_host_info` | Gauge | `pi_type` | Resolved host metadata (value=1) |
| `picolet_host_feature_info` | Gauge | `feature` | Resolved host feature metadata (value=1) |
| `picolet_secrets_managed_count` | Gauge | `provider` (`onepassword`/`protonpass`) | Number of direct provider-backed secret refs currently managed |
| `picolet_secret_sync_total` | Counter | `provider` | Successful secret-provider sync attempts (failures counted on `picolet_reconciliation_total{result="failure"}`) |
| `picolet_secret_last_sync_timestamp` | Gauge | `provider` | Unix timestamp of the last successful secret-provider sync |
| `picolet_secret_credential_expires_at` | Gauge | `provider` | Unix timestamp at which the configured credential expires (only emitted when the operator records the expiry in config) |

Dependency targets, file paths, hashes, recent error strings, and dashboard event history are intentionally not exported as labels to avoid high-cardinality or churn-heavy series.

> **Self-update monitoring:** use node-exporter's `systemd_unit_start_time_seconds` for `picolet.service` to detect restart failures.

## Upgrading from 1Password-only metrics

The previous `picolet_op_*` metric family has been replaced with a provider-labeled family. Existing dashboards and rules need to be remapped:

| Old (removed) | New |
|---|---|
| `picolet_op_direct_secrets_count` | `picolet_secrets_managed_count{provider="onepassword"}` |
| `picolet_op_sync_total` (`result="success"` only) | `picolet_secret_sync_total{provider="onepassword"}` (the `result` label is dropped; failures live on `picolet_reconciliation_total{result="failure"}`) |
| `picolet_op_last_sync_timestamp` | `picolet_secret_last_sync_timestamp{provider="onepassword"}` |

The new `picolet_secret_credential_expires_at{provider}` gauge is opt-in: it is only emitted when `onepassword.token_expires_at` or `protonpass.pat_expires_at` is set in `config.yml`. Use `absent_over_time(picolet_secret_credential_expires_at{provider="..."}[1h])` to flag providers whose expiry was never declared.

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

      - alert: PicoletSecretCredentialNearExpiry
        expr: picolet_secret_credential_expires_at - time() < 14 * 86400
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.provider }} credential expires in less than 14 days"
          description: "Rotate the {{ $labels.provider }} credential on {{ $labels.instance }} and update token_expires_at / pat_expires_at in config.yml."

      - alert: PicoletSecretCredentialExpired
        expr: picolet_secret_credential_expires_at - time() < 0
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.provider }} credential has expired"
          description: "Secret resolution for {{ $labels.provider }} on {{ $labels.instance }} will fail until the credential is rotated."
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

## Reloading Rule And Scrape Config

When a service supports hot reload, place a `picolet.yml.tmpl` file in the same
service bundle as the secret. The snippet uses Go template syntax (`{{ index
.Ports "vmalert" }}`), which only works in a `.tmpl` file — do not copy it into
a plain `picolet.yml`. Example for vmalert:

```yaml
# services/vmalert/picolet.yml.tmpl
hooks:
  - name: vmalert-rules
    secrets: [vmalert_rules]
    unit: vmalert.service
    action: http
    method: GET
    url: 'http://localhost:{{ index .Ports "vmalert" }}/vmalert/-/reload'
    health_url: 'http://localhost:{{ index .Ports "vmalert" }}/vmalert/health'
```

Use `action: restart` for services where the running process cannot see replaced
Podman secret content without a new container. Use `action: signal` with
`signal: HUP` for daemons that reload config on SIGHUP.

## Migration From Monolithic Rules

You can keep the same secret assignment and migrate incrementally:

1. Keep the existing secret file name (for example `secrets/prometheus_rules.yml.tmpl`).
2. Replace monolithic inline groups with `concatFiles`:

```yaml
groups:{{ concatFiles "rules/prometheus/*.yml" | nindent 2 }}
```

3. Move each alert group into its own file under `rules/prometheus/`.
4. Keep backend-specific semantic checks in fleet CI (Prometheus, VictoriaMetrics/vmalert, etc.).
