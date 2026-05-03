# Picolet Dashboard

Living document for the picolet dashboard. v1 ships in PR #N (GitHub Issue #10) — a single-route, read-only HTML page served from the existing metrics HTTP server. This document tracks intended v2 follow-ups; nothing here is implemented yet.

---

## v2 Followups

### 1. Dependency view (highest-value follow-up)

Surface declared and Quadlet-auto-generated systemd dependencies (`Requires`, `Wants`, `After`, `Before`, `BindsTo`, `PartOf`) per unit.

The data is already produced and discarded today: the validator runs `quadlet.Convert*()` in `pkg/validator/quadlet.go`, which returns `*parser.UnitFile` with a fully parsed `[Unit]` section including Quadlet's *implicit* deps (e.g. `Network=foo` in a `.container` produces `After=foo-network.service` in the generated unit). This makes the parsed `UnitFile` the right source — raw quadlet sources don't show implicit deps.

Approach:
- Refactor `pkg/validator` so the validator returns `map[serviceName]ParsedDeps` alongside its existing error result, or expose a separate `ExtractDeps` function reusing `buildUnitsInfoFromFiles` in `pkg/validator/validator.go`.
- `Agent` stores the latest dep map in-memory only — recomputable on next reconcile, never persisted to `state.json`.
- Dashboard handler joins deps onto each unit row.
- Template adds `<details><summary>dependencies</summary><ul>…</ul></details>` per row. Style `<details>` to match the existing aesthetic — disclosure triangle replaced with `+`/`−` glyph in `--accent`, indented under the row in dim slate.

Explicit non-goal: graph visualization (SVG / DAG layout). Plain text per row is sufficient signal at fleet scale.

### 2. Actions (reconcile / suspend / resume)

Operator-initiated triggers from the dashboard. Issues to think through before building:

- **Auth model**: today the dashboard is anonymous, matching `/metrics`. Actions need at minimum CSRF protection plus something stronger than network trust. Options: basic auth (simplest), per-host bearer token in agent config, OIDC (overkill for one-binary agents).
- **Action surface**: trigger reconcile (already exists via `/webhook` with HMAC — could expose a separately-tokened browser-friendly endpoint), suspend reconciliation (new — needs a flag in `state.State`), resume.
- **Audit log**: who triggered what, when. Could be a ring buffer in memory or appended to a log file.

### 3. Reconciliation history

Circular buffer of the last N reconciles in `state.State` (or a sidecar JSON to keep `state.json` lean). Each entry: SHA, timestamp, success/failure, files changed, restart count, errors. Bounded size.

Surface as a "Recent reconciles" panel on the dashboard.

### 4. Drill-down per unit

Route `/units/<name>` showing per-unit details: full file path, latest content hash, last apply time, last `journalctl` snippet, last healthcheck result, dependencies, dependents.

Requires lifting state beyond what's persisted today (e.g. last-error per unit, last apply time per unit). Likely paired with #1 (dep view) to show dependents inline.

### 5. Search / filter / live updates (HTMX)

If the fleet grows or we add detail pages, lift the page from "plain HTML" to "HTMX-augmented":
- Filter by category, status, name (client-side initially; server-side if we paginate).
- Live status updates — poll a JSON sub-endpoint via `hx-get` every N seconds, swap just affected `<tr>` elements.
- No SPA framework. Picolet stays one binary.

### 6. Orphan view

`pkg/orphan` already detects stale managed files/secrets at startup. Surface what was cleaned up (or what would be cleaned up under a dry-run mode) in a panel.

### 7. Recent apply errors

`ApplyResult.Errors` is logged today but ephemeral. Persist last-apply errors briefly in an in-memory ring buffer and render in a "Recent issues" panel.

### 8. Configurability

- Auto-refresh interval as an `agentcfg` field.
- Disable dashboard via config flag (currently always-on if HTTP server is up).
- Mount under a configurable subpath (e.g. `/ui/`) instead of `/`.
- Explicit dark/light theme toggle (currently auto via `prefers-color-scheme`; a manual override would need a JS-set `data-theme` attribute and `localStorage`, breaking the no-JS rule — would land alongside HTMX).

### 9. Distinctive monospace typeface

v1 ships with the system monospace stack (SF Mono / Cascadia Code / Liberation Mono depending on OS). For consistent cross-platform aesthetic — especially on Linux desktops — vendor a distinctive variable monospace as a Latin-subset woff2: candidates are Commit Mono (MIT), IBM Plex Mono (OFL), Sometype Mono. Subset via `pyftsubset` to ~50 KB. Adds NOTICE + license attribution. Originally part of v1 but pulled per review — font subsetting is the highest-churn slice of the asset pipeline and out of scope for "super simple".

### 10. Host metadata in header (`pi_type`, `features`)

Surface the resolved `HostConfig` (`pkg/config/host.go`: `PiType`, `Features`) in the dashboard header. Requires either (a) re-resolving fleet config on each dashboard request (cheap, repo is local — but couples dashboard to resolver) or (b) caching the last-resolved `*HostConfig` on the `Agent` and exposing it to the dashboard via the same `WithDashboard` wiring. (b) is cleaner.

### 11. Live in-memory liveness signal

`state.LastSuccessfulReconciliationAt` is only persisted on apply; the noop fast path updates it in memory only. The dashboard reads `state.json` and so can lag the actual last-OK by minutes. Expose an in-memory snapshot via the `Agent` (or a tiny new "status" pkg) so the dashboard can show a true "last verified OK" timestamp distinct from "last applied". Pairs naturally with #10 since both involve the agent exposing live state.
