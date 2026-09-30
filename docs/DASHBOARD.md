# Picolet Dashboard

Living document for the picolet dashboard. v1 ships in PR #69 (GitHub Issue #10) — a single-route, read-only HTML page served from the existing metrics HTTP server.

v2 implements the read-only runtime slice: dependency disclosure, host metadata, live verified-OK time, refresh accessibility, orphan cleanup results, recent in-memory events/issues, and Prometheus-safe summary metrics.

## Status

Shipped: dependency view, host metadata in the header, `?refresh=0` accessibility, live verified-OK signal.
Orphan view: the panel renders only when something was cleaned up.
Unit rows of timer-triggered one-shots show "last success … ago" and, when the unit's current `Result=` is not `success`, that result (GitHub issue #160) — a pure passthrough of the status store's run record, the same data the `picolet_unit_last_*` metrics export.

## Freeze

The built-in dashboard is frozen: **no new features**. The read-only view model tracks what the status store holds — a field added to the store may surface in the view, but the dashboard grows no actions, history, drill-down pages, configuration or client-side framework. Anything beyond that belongs in a dashboard over picolet's Prometheus metrics (see [alerting](alerting.md)), which is what the production Fleet runs.

## Removal criterion

The built-in dashboard is removed once a reference dashboard for the metrics exists in the reference fleet (`deploy/fleet-repo/`).
