# Alerting & Metrics Reference

## Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `picolet_reconciliation_total` | Counter | `result` (success/failure/noop/retry_pending/paused) | Total reconciliation attempts |
| `picolet_reconciliation_duration_seconds` | Histogram | — | Duration of reconciliation cycles |
| `picolet_last_successful_reconciliation_timestamp` | Gauge | — | Unix timestamp of last successful reconciliation |
| `picolet_git_poll_total` | Counter | `result` (changed/noop/error/secret_refresh/pending_hook_retry/pending_unit_retry) | Total git poll attempts |
| `picolet_files_applied_total` | Counter | `action`, `category` | Files applied per action (create/update/delete) and category |
| `picolet_files_managed_total` | Gauge | `category` | Current managed files by category |
| `picolet_failed_sha_consecutive_count` | Gauge | — | Consecutive failures for current SHA (gates at 3) |
| `picolet_rollback_total` | Counter | — | Total rollbacks performed |
| `picolet_health_check_total` | Counter | `unit`, `result` | Health checks by unit and result |
| `picolet_health_enforcement_total` | Counter | `unit`, `action` (restart/skip_cooldown/skip_external_activation) | Health enforcement actions by unit (`skip_external_activation`: a failed one-shot systemd owns — timer-triggered or static — reported but not restarted) |
| `picolet_systemd_unit_operations_total` | Counter | `operation` (enable/disable/start/restart), `result` (success/error/skipped) | Enable/disable/start/restart operations on raw systemd units and one-shot restart gating (`result="skipped"`: a restart declined because the unit is a timer-triggered one-shot) |
| `picolet_unit_restart_pending` | Gauge | `unit` | Managed units whose last restart attempt failed (value = consecutive failed attempts). Seeded from persisted state, so it survives an agent restart |
| `picolet_unit_last_run_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a timer-triggered one-shot last started, whatever the outcome. Absent until the first run, so `absent()` distinguishes "never fired" from "fired and failed" |
| `picolet_unit_last_success_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a timer-triggered one-shot last completed successfully. Absent until the first observed success — never zero, so `time() - series` is never poisoned by the epoch |
| `picolet_unit_last_result` | Gauge | `unit`, `result` (`success`/`exit-code`/`timeout`/`signal`/…) | Info metric (value=1) for the unit's **current** systemd `Result=`, one series per unit. systemd resets `Result=` to `success` when a run starts, so while a job is in flight this reads `success` even if the previous run failed — join with `picolet_unit_last_success_timestamp_seconds` when only a completed outcome counts. Absent until the unit has run at all |
| `picolet_timer_last_trigger_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a managed `.timer` last fired |
| `picolet_applied_git_sha_info` | Gauge | `sha` | Currently applied git SHA (value=1) |
| `picolet_orphans_removed_total` | Counter | `type` (file/secret) | Orphaned resources removed at startup |
| `picolet_unit_dependency_count` | Gauge | `unit`, `relation` | Current generated systemd dependency count by managed unit and relation |
| `picolet_host_info` | Gauge | `role` | Resolved host metadata (value=1) |
| `picolet_host_feature_info` | Gauge | `feature` | Resolved host feature metadata (value=1) |
| `picolet_secrets_managed_count` | Gauge | `provider` (`onepassword`/`protonpass`) | Number of direct provider-backed secret refs currently managed |
| `picolet_secret_sync_total` | Counter | `provider` | Successful secret-provider sync attempts (failures counted on `picolet_reconciliation_total{result="failure"}`) |
| `picolet_secret_last_sync_timestamp` | Gauge | `provider` | Unix timestamp of the last successful secret-provider sync |
| `picolet_secret_credential_expires_at` | Gauge | `provider` | Unix timestamp at which the configured credential expires (only emitted when the operator records the expiry in config) |

Dependency targets, file paths, hashes, recent error strings, and dashboard event history are intentionally not exported as labels to avoid high-cardinality or churn-heavy series.

> **Scheduled jobs:** the four `picolet_unit_last_*` / `picolet_timer_last_trigger_*` series cover every Managed unit picolet classifies as a timer-triggered one-shot (`Type=oneshot` with a `.timer` in `TriggeredBy=`) plus the timers that fire them. They are read from systemd on every health pass and retained across a failed D-Bus query, so a hiccup does not make them flap. `picolet_unit_last_result` is systemd's live `Result=`; last-success is *derived* on top of it, because systemd resets `Result=` to `success` when a run starts and keeps no success history. Consequences: a job that has not succeeded since the agent restarted reports no last-success series (the never-succeeded rule below covers it), and the last-success value never advances on the strength of a run picolet did not see finish. `picolet_last_image_prune_timestamp` is unchanged and still covers picolet's own pruning.

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

      - alert: PicoletUnitRestartPending
        expr: picolet_unit_restart_pending > 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} has been failing to restart"
          description: "picolet on {{ $labels.instance }} has not been able to restart {{ $labels.unit }} ({{ $value }} consecutive failed attempts). Check the unit's quadlet and `pending_units` in state.json."

      # picolet exports the timestamps, not the OnCalendar= interval: pick the
      # threshold per schedule class (2x the interval) with a unit matcher, and
      # duplicate the rule for jobs on a different schedule. A daily job:
      - alert: PicoletScheduledJobStale
        expr: time() - picolet_unit_last_success_timestamp_seconds > 2 * 86400
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} has not succeeded for over two days"
          description: "The timer-triggered one-shot {{ $labels.unit }} on {{ $labels.instance }} last succeeded {{ $value | humanizeDuration }} ago. Check `picolet_unit_last_result` for why and `journalctl -u {{ $labels.unit }}`."

      # A job that has fired but never once succeeded has no last-success series
      # at all, so the staleness rule above cannot see it.
      - alert: PicoletScheduledJobNeverSucceeded
        expr: picolet_unit_last_run_timestamp_seconds unless on(instance, unit) picolet_unit_last_success_timestamp_seconds
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} has never completed successfully"
          description: "{{ $labels.unit }} on {{ $labels.instance }} has run at least once and has never been observed to succeed."

      # picolet_unit_last_result is systemd's live Result=, so this clears while the
      # next attempt runs and re-fires if that attempt fails too. Use the staleness
      # rule above for the stable "has stopped succeeding" signal.
      - alert: PicoletScheduledJobFailed
        expr: picolet_unit_last_result{result!="success"} == 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} last run ended in {{ $labels.result }}"
          description: "The last run of {{ $labels.unit }} on {{ $labels.instance }} ended with Result={{ $labels.result }}."

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
