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
| `picolet_health_enforcement_total` | Counter | `unit`, `action` (restart/skip_cooldown/skip_external_activation) | Health enforcement actions by unit (`skip_external_activation`: a failed unit picolet reports but does not restart — a timer-triggered or static one-shot, or a `.build`/`.image` service that failed on its own; a `.image` whose apply-time pull failed is retried and counts as `restart`) |
| `picolet_unit_active` | Gauge | `unit` | 1 if the managed unit is active or activating, 0 if failed. Absent for inactive and transitional states, so `== 0` fires only for failed units (a finished one-shot or `.build` is inactive, not failed) |
| `picolet_unit_state_info` | Gauge | `unit`, `active_state`, `sub_state` | Info metric (value=1) for every managed unit's current systemd state, inactive included. `active_state="failed"` is always a problem. `"inactive"` is expected for one-shots between runs and for a finished `.build`, but for a long-running service it means stopped, so read it together with the unit type |
| `picolet_systemd_unit_operations_total` | Counter | `operation` (enable/disable/start/restart), `result` (success/error/skipped) | Enable/disable/start/restart operations on raw systemd units and one-shot restart gating (`result="skipped"`: a restart declined because the unit is a timer-triggered one-shot) |
| `picolet_unit_restart_pending` | Gauge | `unit` | Managed units whose last restart attempt failed (value = consecutive failed attempts). Seeded from persisted state, so it survives an agent restart |
| `picolet_unit_last_run_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a timer-triggered one-shot last started, whatever the outcome. Absent until the first run, so `absent()` distinguishes "never fired" from "fired and failed" |
| `picolet_unit_last_success_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a timer-triggered one-shot last completed successfully. Absent until the first observed success — never zero, so `time() - series` is never poisoned by the epoch |
| `picolet_unit_last_result` | Gauge | `unit`, `result` (`success`/`exit-code`/`timeout`/`signal`/…) | Info metric (value=1) for the unit's **current** systemd `Result=`, one series per unit. systemd resets `Result=` to `success` when a run starts, so while a run is in flight this reads `success` even if the previous run failed — join with `picolet_unit_last_success_timestamp_seconds` when only a completed outcome counts. Absent until the unit has run at all |
| `picolet_timer_last_trigger_timestamp_seconds` | Gauge | `unit` | Unix timestamp at which a managed `.timer` last fired |
| `picolet_applied_git_sha_info` | Gauge | `sha` | Currently applied git SHA (value=1) |
| `picolet_orphans_removed_total` | Counter | `type` (file/secret) | Orphaned resources removed at startup |
| `picolet_unit_dependency_count` | Gauge | `unit`, `relation` | Current generated systemd dependency count by managed unit and relation |
| `picolet_host_info` | Gauge | `role`, `machine`, `user` (`root` for the rootful Host) | Resolved host metadata (value=1); join on it to group a Machine's Agents, e.g. `picolet_reconciliation_total * on(instance) group_left(machine, user) picolet_host_info` |
| `picolet_host_feature_info` | Gauge | `feature` | Resolved host feature metadata (value=1) |
| `picolet_secrets_managed_count` | Gauge | `provider` (`onepassword`/`protonpass`) | Number of direct provider-backed secret refs currently managed |
| `picolet_secret_sync_total` | Counter | `provider` | Successful secret-provider sync attempts (failures counted on `picolet_reconciliation_total{result="failure"}`) |
| `picolet_secret_last_sync_timestamp` | Gauge | `provider` | Unix timestamp of the last successful secret-provider sync |
| `picolet_secret_credential_expires_at` | Gauge | `provider` | Unix timestamp at which the configured credential expires (only emitted when the operator records the expiry in config) |

Dependency targets, file paths, hashes, recent error strings, and dashboard event history are intentionally not exported as labels to avoid high-cardinality or churn-heavy series.

> **Timer-triggered one-shots:** the four `picolet_unit_last_*` / `picolet_timer_last_trigger_*` series cover every Managed unit picolet classifies as a timer-triggered one-shot (`Type=oneshot` with a `.timer` in `TriggeredBy=`) plus the timers that fire them. They are read from systemd on every health pass and retained across a failed D-Bus query, so a hiccup does not make them flap. `picolet_unit_last_result` is systemd's live `Result=`; last-success is *derived* on top of it, because systemd resets `Result=` to `success` when a run starts and keeps no success history. Consequences: a one-shot whose last run before an Agent restart did not succeed (or was still running) reports no last-success series until it next succeeds, and the last-success value never advances on the strength of a run picolet did not see finish. The dashboard shows the same record on the unit's row: "last success … ago" and, when the current `Result=` is not `success`, that result. See [Alerting on timer-triggered one-shots](#alerting-on-timer-triggered-one-shots) for rules.

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

## Alerting on timer-triggered one-shots

Alert on the outcome that matters — **no successful run for twice the schedule
interval** — rather than on each failure: one missed or slow run stays quiet, a
second consecutive miss fires. picolet exports timestamps, not the `OnCalendar=`
schedule, so write the threshold per unit (or per schedule class, with a regex
`unit` matcher).

The staleness expression evaluates only while a last-success series exists. Pair
it with an `absent_over_time()` companion over the same window, so a series that
vanished or never appeared also fires: the one-shot has never succeeded; the
Agent restarted after a run that did not succeed or was still in flight
(last-success lives in memory; after a restart it is re-derived only from a run
systemd reports as finished with `Result=success`) and none has
succeeded since; the Machine rebooted and the one-shot has not succeeded since
(see below); or picolet is not being scraped. Removing only the `.timer` from the
Fleet does not make the series vanish — the `.service` is still managed, so its
last-success stays frozen and the staleness rule fires. When the one-shot itself
leaves the Fleet its series goes away, so remove its rules with it.
`absent_over_time()` knows only the labels its selector pins with `=`: pin
`unit`, and also pin `instance` when the one-shot runs on several Hosts and each
must be checked — without it the companion fires only when *no* Host reports the
series.

Example: a daily `backup.service` and a weekly `restore-verify.service`.

```yaml
groups:
  - name: picolet-one-shots
    rules:
      # backup.service — OnCalendar=daily, so 2 days.
      - alert: PicoletOneShotStale
        expr: time() - picolet_unit_last_success_timestamp_seconds{unit="backup.service"} > 2 * 86400
        for: 15m
        labels:
          severity: warning
          schedule: daily
        annotations:
          summary: "{{ $labels.unit }} has not succeeded for over two days"
          description: "{{ $labels.unit }} on {{ $labels.instance }} last succeeded {{ $value | humanizeDuration }} ago. Check `picolet_unit_last_result` for why and `journalctl -u {{ $labels.unit }}`."

      - alert: PicoletOneShotSuccessAbsent
        expr: absent_over_time(picolet_unit_last_success_timestamp_seconds{unit="backup.service"}[2d])
        labels:
          severity: warning
          schedule: daily
        annotations:
          summary: "No success of {{ $labels.unit }} reported for two days"
          description: "No Host has reported a successful run of {{ $labels.unit }} for two days: it has never succeeded, has not succeeded since an Agent or Machine restart, or picolet is not being scraped."

      # restore-verify.service — OnCalendar=weekly, so 14 days.
      - alert: PicoletOneShotStale
        expr: time() - picolet_unit_last_success_timestamp_seconds{unit="restore-verify.service"} > 14 * 86400
        for: 15m
        labels:
          severity: warning
          schedule: weekly
        annotations:
          summary: "{{ $labels.unit }} has not succeeded for over two weeks"
          description: "{{ $labels.unit }} on {{ $labels.instance }} last succeeded {{ $value | humanizeDuration }} ago. Check `picolet_unit_last_result` for why and `journalctl -u {{ $labels.unit }}`."

      - alert: PicoletOneShotSuccessAbsent
        expr: absent_over_time(picolet_unit_last_success_timestamp_seconds{unit="restore-verify.service"}[14d])
        labels:
          severity: warning
          schedule: weekly
        annotations:
          summary: "No success of {{ $labels.unit }} reported for two weeks"
          description: "No Host has reported a successful run of {{ $labels.unit }} for two weeks: it has never succeeded, has not succeeded since an Agent or Machine restart, or picolet is not being scraped."

      # Unit-agnostic: a one-shot that has run but has not been seen to succeed
      # since the Agent started. Needs no per-unit rule.
      - alert: PicoletOneShotNeverSucceeded
        expr: picolet_unit_last_run_timestamp_seconds unless on(instance, unit) picolet_unit_last_success_timestamp_seconds
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} has not succeeded since the Agent started"
          description: "{{ $labels.unit }} on {{ $labels.instance }} has run at least once and has not been observed to succeed."

      # Optional, faster and noisier: picolet_unit_last_result is systemd's live
      # Result=, so this clears while the next attempt runs and re-fires if that
      # attempt fails too. The staleness pair above is the stable signal.
      - alert: PicoletOneShotFailed
        expr: picolet_unit_last_result{result!="success"} == 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.unit }} last run ended in {{ $labels.result }}"
          description: "The last run of {{ $labels.unit }} on {{ $labels.instance }} ended with Result={{ $labels.result }}."
```

> **Reboots and `Persistent=true`:** systemd keeps a one-shot's run timestamps
> and `Result=` in memory, so after a reboot it has no memory of the pre-reboot
> run: picolet exports no run, success or result series for the unit until it
> runs again, and only the `absent_over_time()` companion can fire in that gap.
> A timer with `Persistent=true` records its last trigger on disk, so systemd
> remembers it across the reboot — and, if a scheduled run fell into the
> downtime, starts the one-shot right after boot as a catch-up run. That run
> competes with everything else starting at boot (a backup while containers are
> still coming up), so `Persistent=true` is an opt-in the Fleet author makes
> knowingly per timer; picolet never adds it.

> **Image prune:** `picolet_last_image_prune_timestamp` remains and still covers
> picolet's in-process prune (`prune_images` / `prune_interval`). A prune run
> as a timer-triggered one-shot instead (the README's maintenance timer) is
> covered by the generic series above like any other one-shot.

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
