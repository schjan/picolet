# Expose the host's systemd unit list to templates

**Status:** Draft
**Date:** 2026-05-21
**Author:** Jannis Schäfer
**Scope:** picolet feature; small consumer change in fleet repos that want to use it

## Background

Several monitoring and diagnostic tools need to filter or enumerate the
systemd units running on a host. The motivating case is Prometheus
`node-exporter`, whose `--collector.systemd.unit-include` flag takes a regex.
In fleet repos using picolet today, this regex is hardcoded in the
quadlet template and must be updated by hand whenever a node gains or loses
a service. Example from the `iuk-gitops` fleet repo:

```
--collector.systemd.unit-include=^(picolet.*|podman.*|node-exporter|mosquitto|victoriametrics|vmalert|...|tailscale|dbus|systemd-networkd|systemd-resolved)[.](service|socket|timer)$
```

Picolet already knows which services and systemd units it deploys per host —
this knowledge is just not surfaced to templates. The same information
would let any fleet template generate per-host unit lists dynamically.

## Goals

- Expose the host's resolved bundle list as template data.
- Expose the host's derived systemd unit names as template data, including
  units whose names come from `ServiceName=` overrides or from quadlets
  that don't follow the bundle-name == unit-name convention.
- Keep the change small enough that adopters can replace a hardcoded regex
  with a one-time template edit and never touch the regex again.
- Make the two-pass rendering pattern (already used for secret refs) into a
  first-class concept in the resolver so future deferred-data needs plug in
  cleanly.
- Backward-compatible: both new fields are additive. Existing templates and
  fleet repos continue to render unchanged.

## Non-goals

- No new bundle metadata (e.g. `extra_units:` in `picolet.yml`). The
  filename-based rule for raw systemd units covers the cases we have.
- No support for templates whose own identity (filename, ServiceName=,
  ContainerName=) depends on `.Host.SystemdUnits`. That is a self-reference
  cycle the two-pass design cannot resolve.
- No changes to picolet's state schema, metrics, hooks, or apply pipeline.
  This is purely a resolver-time / template-time addition.

## Design overview

### Two-pass rendering, explicitly

Some template data only becomes knowable after `.tmpl` files render. Picolet
already two-passes for secrets: it renders once with placeholder values to
collect `op://` and `pass://` refs, batch-resolves them per provider, and
renders again with real secrets. This spec generalises that pattern into a
named **first pass** that also discovers systemd unit names.

```
ResolveHost(host):
  1. Resolve assignments → ResolvedFileSet            (existing)
     └─ host's bundle list known here → .Host.Services

  2. Build initial TemplateData
     {Services: populated, SystemdUnits: nil}

  3. prepareTemplateData (FIRST PASS):                (NEW orchestrator)
     - Render every .tmpl with placeholder data:
       · secret refs (op://, pass://) populate per-provider caches
       · rendered quadlet content parsed → unit names collected
     - Read non-template quadlets (.container, .kube) directly,
       parse for unit names
     - Derive unit names from raw systemd filenames (.socket, .timer)
     - ResolveAll secret caches
     - Output: sorted, unique SystemdUnits []string

  4. Populate TemplateData.Host.SystemdUnits

  5. Final render pass                                (existing)
     - All templates rendered with full TemplateData
     - Errors propagate; this is the diagnostic source of truth
```

### Why two passes are necessary

A quadlet's systemd unit name is derived from its **rendered** content via
Podman's `quadlet.GetUnitServiceName()`. To expose the full unit list to
**other** templates (e.g. a node-exporter scrape config that filters by
unit name), every quadlet must be rendered at least once before any
template that consumes `.Host.SystemdUnits` produces its final output.

The first pass is the cheapest possible solution to this cycle: one extra
render per template, no extra IO, no extra network calls. Performance is
negligible compared to git fetches and secret-provider round-trips.

## API additions

### `pkg/resolver/templatedata.go`

```go
// HostTemplateData holds per-host template variables.
type HostTemplateData struct {
    Hostname         string
    ExternalHostname string
    Role             string
    Features         []string

    // Services is the resolved bundle name list for this host, merged from
    // assignments.yml (base + role + features). Sorted and deduplicated.
    // Mirrors what Assignments.Resolve(host).Services returns.
    Services []string

    // SystemdUnits is the list of systemd unit names picolet manages on
    // this host. Sorted and deduplicated. Includes:
    //   - Quadlet-derived units (.container, .kube) via Podman's parser,
    //     which handles ServiceName= overrides and multi-container bundles.
    //   - Raw systemd units (CategorySystemd), where the unit name is the
    //     filename with any .tmpl suffix stripped (e.g. "https.socket").
    // Populated by the first render pass — see prepareTemplateData in
    // pkg/resolver/resolver.go.
    SystemdUnits []string
}
```

Both fields are nil-safe (zero-length slice is valid template input).

### `pkg/resolver/resolver.go`

A new orchestrator function with the umbrella doc comment that explains the
two-pass design:

```go
// preparedData is the output of the first render pass.
//
// Some template data only becomes knowable AFTER quadlet templates render:
//   - Secret refs (op://, pass://) — providers batch better when picolet
//     knows all refs upfront, so we collect them first and resolve in bulk.
//   - Systemd unit names — a quadlet's unit name is derived from its
//     rendered content via Podman's parser, so .Host.SystemdUnits can only
//     be populated AFTER every quadlet has been rendered at least once.
//
// The first pass renders every .tmpl with placeholder data (nil/empty for
// the not-yet-discovered fields), harvests these side effects, and
// discards the output. The second (final) pass renders for real with
// fully populated TemplateData. Render errors in the first pass are
// non-fatal; the final pass surfaces real template errors with proper
// diagnostics.
//
// Cost: one extra render per template. Negligible vs. git fetches and
// secret-provider round-trips.
type preparedData struct {
    SystemdUnits []string // sorted, unique; e.g. "node-exporter.service"
    // Secret caches are mutated in place via provider functions during
    // the first-pass render — not returned here.
}

func (r *Resolver) prepareTemplateData(
    ctx context.Context,
    registry *template.Template,
    tmplData *TemplateData,
    expanded *expandedResult,
    caches ProviderCaches,
) (*preparedData, error)
```

The existing `runTemplateRefCollection` is folded into this orchestrator.
`collectTemplateRefs` remains as a focused helper called by the orchestrator
for the secret-collection side effect.

## Behavior rules

### Discovery rule per file category

| Source | Unit name derivation |
|---|---|
| `.container.tmpl`, `.kube.tmpl` | First-pass render → Podman parser → `unitServiceName()` (existing helper appends `.service`) |
| `.container`, `.kube` (static) | Read directly → Podman parser → `unitServiceName()` |
| `.socket`, `.timer`, … in `CategorySystemd` | Filename with `.tmpl` suffix stripped |
| Manifests, files, secrets, volumes, networks | Not included in `SystemdUnits` |

### Sorting and dedup

Picolet sorts and deduplicates both `Services` and `SystemdUnits` before
exposing them. Templates that need a different order can re-sort. This
matches the existing convention in `ResolvedFileSet`.

### Determinism

For a given `(assignments.yml, hosts/<name>/host.yml, services/...)` input,
`Services` and `SystemdUnits` are deterministic across runs.

## Edge cases and error model

### Self-referencing templates

A template that references `.Host.SystemdUnits` in itself produces correct
output **iff** it does not break when `SystemdUnits` is empty in the first
pass.

| Usage pattern | Pass 1 behavior | Final result |
|---|---|---|
| `range .Host.SystemdUnits` | Empty range, renders fine | Own unit name in final SystemdUnits ✅ |
| `index .Host.SystemdUnits 0` | Template error, output discarded | Own unit name missing from final SystemdUnits ❌ |
| Conditional on `len .Host.SystemdUnits` affecting filename/ServiceName= | Renders pass-1 branch, name collected | Final pass may render differently — inconsistency ❌ |

The recommended pattern is `range`. Templates that need to test `len` for
non-identity-affecting reasons (e.g. emitting a default if empty) are safe.

### Error model

| Stage | Failure mode | Behavior |
|---|---|---|
| First pass — template render | Per-template error | Swallowed; final pass surfaces with diagnostics |
| First pass — quadlet parse | Per-file error | Skipped; file's unit name absent from `SystemdUnits`; validator catches the parse error |
| First pass — `caches.ResolveAll` | Aggregate error | Propagated (existing behavior) |
| Final pass | Any error | Propagated; this is the source of truth for diagnostics |

### Validate / CI mode

`picolet validate` runs in placeholder secret mode. The first pass renders
quadlets with placeholder secret values. Quadlet structure does not depend
on secret values, so parsing succeeds and `SystemdUnits` is populated
correctly. This makes the rendered output of consumers (e.g. node-exporter
templates) verifiable in CI.

### Host isolation

The resolver runs per host. Each host has its own `TemplateData` and its
own first pass. Hosts do not see each other's `SystemdUnits`.

## Consumer-side change (fleet repo)

The node-exporter template in `iuk-gitops`
(`services/node-exporter/containers/node-exporter.container.tmpl`) becomes:

```gotemplate
[Unit]
Description=Prometheus Node Exporter
After=network-online.target
Wants=network-online.target

[Container]
Image={{ index .Images "node_exporter" }}
ContainerName=node-exporter
Network=host
ReadOnly=true
Volume=/proc:/host/proc:ro
Volume=/sys:/host/sys:ro
Volume=/run/dbus/system_bus_socket:/var/run/dbus/system_bus_socket:ro
Volume=/run/udev/data:/run/udev/data:ro
SecurityLabelDisable=true
{{- /* Build the unit-include regex alternation:
       - picolet-managed unit basenames from .Host.SystemdUnits
       - host-level units and patterns picolet doesn't manage. */ -}}
{{- $managed := list -}}
{{- range .Host.SystemdUnits -}}
  {{- $base := trimSuffix ".service" . | trimSuffix ".socket" | trimSuffix ".timer" -}}
  {{- $managed = append $managed $base -}}
{{- end -}}
{{- $hostLevel := list "picolet.*" "podman.*" "tailscale" "dbus" "systemd-networkd" "systemd-resolved" -}}
{{- $alternation := concat $managed $hostLevel | uniq | sortAlpha | join "|" -}}
Exec=--path.procfs=/host/proc --path.sysfs=/host/sys --collector.systemd --collector.systemd.unit-include=^({{ $alternation }})[.](service|socket|timer)$ --web.listen-address=:{{ index .Ports "node_exporter" }} --log.format=json

HealthCmd=CMD-SHELL wget -q -O /dev/null http://localhost:{{ index .Ports "node_exporter" }}/
HealthInterval=30s
HealthRetries=3
HealthStartPeriod=30s
HealthTimeout=5s

[Service]
Restart=always
RestartSec=10s

[Install]
WantedBy=default.target
```

Notes:

- `picolet.*` and `podman.*` remain as regex patterns. `podman.*` catches
  systemd-generated podman maintenance units that picolet does not manage
  (and therefore does not appear in `.Host.SystemdUnits`). `picolet.*` is
  defense-in-depth — `.Host.SystemdUnits` already includes `picolet.service`
  and `picolet-system.service`, so the pattern is technically redundant but
  cheap.
- `sortAlpha | uniq` keeps the rendered regex deterministic across runs, so
  diffs remain noise-free when unrelated services move in/out.
- Available sprig functions used: `list`, `append`, `concat`, `uniq`,
  `sortAlpha`, `join`, `trimSuffix`.

## Test plan

### Picolet — unit tests in `pkg/resolver`

| Test | Pins down |
|---|---|
| `TestTemplateDataServices` | `.Host.Services` matches `Assignments.Resolve(host).Services` |
| `TestTemplateDataSystemdUnits_QuadletDerived` | `.container` and `.kube` produce correct unit names |
| `TestTemplateDataSystemdUnits_ServiceNameOverride` | `ServiceName=` in a quadlet wins over filename-derived name |
| `TestTemplateDataSystemdUnits_RawSystemd` | `.socket`/`.timer` files in `CategorySystemd` appear with their filename-based unit name |
| `TestTemplateDataSystemdUnits_SortedAndUnique` | Output is stable and deduplicated |
| `TestTemplateDataSystemdUnits_SelfReferencing` | A template that `range`s over `.Host.SystemdUnits` sees its own unit name in the final render — guards the actual node-exporter use case |
| `TestTemplateDataSystemdUnits_PassOneErrorsTolerated` | A template that errors on empty `.Host.SystemdUnits` does not break resolution; other unit names still collected |
| `TestTemplateDataSystemdUnits_PlaceholderMode` | Works without a configured secret reader |
| `TestTemplateDataSystemdUnits_NonTemplateQuadlet` | Static (non-`.tmpl`) quadlet files contribute their unit name |
| `TestTemplateDataSystemdUnits_EmptyHost` | Host with no quadlets / no systemd files yields `SystemdUnits == []` |

### Picolet — integration test

Extend `integration_test.go` with a fleet variant that includes a template
referencing `.Host.SystemdUnits`. Use `goldie` to snapshot the rendered
output.

### iuk-gitops

Run `picolet validate` in CI (existing). The new node-exporter template
must validate against `picolet validate` with the current host fixtures.

## Documentation updates

### Picolet repo

| File | Change |
|---|---|
| `README.md` template-data table | Add rows for `.Host.Services` and `.Host.SystemdUnits` |
| `README.md` new "Two-pass rendering" subsection | Concise explainer that lists what is discovered in the first pass and links to the `preparedData` doc comment |
| `CLAUDE.md` Template System section | Update the one-line description: `Host (..., services, systemd_units)` |
| `pkg/resolver/templatedata.go` | Field comments (above) |
| `pkg/resolver/resolver.go` | `preparedData` doc comment (above) is the single source of truth for the rationale |

### iuk-gitops repo

- The node-exporter template gets a short header comment pointing readers at
  picolet's `.Host.SystemdUnits` documentation.
- No other docs change.

## Out of scope (intentionally)

- Bundle metadata for declaring extra units (`extra_units:` in
  `services/<name>/picolet.yml`). Filename-based discovery covers the cases
  we have today.
- Exposing `.Fleet.Assignments` as raw template data. The merged per-host
  `.Host.Services` is the consumable shape; the assignments map itself is
  an internal representation.
- A sprig-style helper that returns unit basenames. Sprig's `trimSuffix`
  composes well enough that a dedicated helper is not worth the API
  surface.
- Cross-host data (e.g. "the list of units on host X" exposed to host Y's
  templates). Fleet-wide queries can be added later if a real use case
  appears; the current `.Fleet.Hosts` data already covers per-host metadata
  without unit lists.
