# Bundle Category for Non-K8s Files (`files/`)

**Status:** approved (brainstorming) — pending implementation plan
**Date:** 2026-05-08
**Scope:** add a new bundle category for opaque, container-mounted files; tighten `manifests/` back to its original Kubernetes-only meaning; remove dead complexity from the manifest path that was added by the hot-reload-hooks ticket (PR #71, commit `639f26d`).

## Problem

The hot-reload-hooks feature added support for declaring `manifests:` as a hook trigger and a `manifestPath` template helper for binding rendered files into containers. The README example demonstrates the feature with a VictoriaMetrics scrape config — i.e., a non-K8s YAML file under `manifests/`.

`pkg/validator/manifest.go` unconditionally requires every file under `manifests/` to be a Kubernetes resource (Pod, Deployment, ConfigMap, …). The documented use case is rejected by `picolet validate` with `missing 'kind' field`.

The conceptual mistake was overloading `manifests/` for two distinct purposes:

1. Kubernetes manifests fed to `podman kube play`.
2. Arbitrary config/data files mounted into containers, where the container is a non-K8s service (Prometheus, vmalert, …).

The name "manifest" carries Kubernetes baggage in every adjacent ecosystem (kubectl, argo, flux). Relaxing the validator to accept anything under `manifests/` papers over the type confusion. The clean fix is a new bundle category for raw files, with `manifests/` strictly K8s.

## Decisions

| Surface | Value |
|---|---|
| Category name (internal) | `file` |
| Bundle subdir | `files/` |
| Hook trigger key | `files:` |
| Template helper | `filePath(relPath)` |
| Deploy path (rootful) | `/var/lib/picolet/files/<rel>` |
| Deploy path (rootless) | `~/.local/share/picolet/files/<rel>` |
| `assignments.yml` top-level field | `files:` (parallel to `manifests:`) |
| Validation | YAML syntax check on `.yml`/`.yaml` (after `.tmpl` strip); otherwise opaque |
| Empty content | allowed |
| Backward compat for non-K8s YAML in `manifests/` | none — clean break |

`manifests/` keeps its Kubernetes-strict validator. K8s reload-via-hook stays a supported pattern (`Hook.Manifests`, `manifestPath` helper, manifest changes as a hook trigger all unchanged).

## Public Surface

### Bundle layout

```
services/<name>/
  containers/
  volumes/
  networks/
  kube/
  systemd/
  secrets/
  manifests/    # K8s YAML only
  files/        # opaque, nested allowed
  picolet.yml
```

`files/` allows nested directories, like `manifests/`. The other six category directories must contain files directly.

### `assignments.yml`

`AssignmentGroup` and `ResolvedFileSet` gain a `Files []string` field, parallel to `Manifests`. Used by non-bundle assignments and merged through the same `merge`/`deduplicate` pipeline.

### Hooks

Hook YAML gains a `files:` trigger key:

```yaml
- name: vm-scrape-reload
  files: [config/scrape.yml]
  unit: victoriametrics.service
  action: http
  method: GET
  url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/-/reload'
  health_url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/health'
```

Hook trigger paths are relative to the **bundle's own** `files/` directory, exactly like `manifests:` is relative to the bundle's `manifests/`. Hooks cannot reference another bundle's files; the hook's bundle owns the path namespace. Hook still requires *at least one* trigger, where `files`, `secrets`, and `manifests` all count.

### Template helper

```
filePath(relPath) → <dataDir>/files/<cleaned>
```

`relPath` must be a clean, non-escaping relative path. Same validation as `manifestPath` today (rejects `..`, leading `/`, `.`, paths that don't equal `path.Clean(...)`). Adapts automatically to rootful/rootless mode via the resolver's `dataDir`.

### Deploy paths

A bundle file at `services/web/files/config/scrape.yml.tmpl` deploys to `/var/lib/picolet/files/config/scrape.yml` (the bundle prefix `services/web/` is stripped; the leading `files/` segment is preserved within `dataDir`). Same scheme as today's manifests.

### Validation

In `pkg/validator/validator.go`, `analyzeFile` gains a `case "file":` arm calling a new `validateFile(f)`:

- Strip `.tmpl` from filename, extract extension.
- If extension is `.yml` or `.yaml`: run `validateYAMLSyntax` on the (rendered) content. This reuses the existing helper used by `validateSecret`.
- Otherwise: return nil.
- Empty content allowed (no empty-content check).

The K8s-strict `validateManifest` is unchanged; misuse of `manifests/` for non-K8s YAML continues to fail with the existing `missing 'kind' field` error.

### Apply order

`categoryOrder` in `pkg/applier/applier.go` becomes:

```
network → volume → secret → systemd → manifest → file → container → kube
```

Files don't trigger daemon-reload, don't have an associated unit, and behave the same way as manifests in the apply loop. The order between `manifest` and `file` is functionally irrelevant; placing `file` immediately after `manifest` reads naturally.

## Internal Layout (Genericization)

The hot-reload-hooks ticket left the manifest path with category-specific scaffolding (`manifestRef`, `readManifestSubdir`, `manifestRelPath()`, `ManifestRelPath` field, `ChangedManifests` map). Naively adding `files/` would duplicate every one of these. Better: genericize the **internal** path while keeping the user-facing surface distinct.

### A. Genericize the nested-bundle walker

`readManifestSubdir(fsys, root, service)` is replaced by `readNestedSubdir(fsys, root, service, category, refSlice *[]bundleFileRef)`. The `bundleSubdirs` registry already tells us which subdirs allow nesting. Both `manifests/` and `files/` flow through the same walker, dispatched on `bundleSubdir.Category`.

### B. Unify `manifestRef` into `bundleFileRef`

Single ref type used for both manifest and file categories:

```go
type bundleFileRef struct {
    SrcPath     string  // repo path with services/<svc>/ prefix (or legacy top-level path)
    LogicalPath string  // src minus services/<svc>/ — preserves leading "manifests/" or "files/"
    Category    string  // "manifest" or "file"
    RelPath     string  // LogicalPath with the category subdir prefix stripped if present;
                        // otherwise LogicalPath unchanged. Matches today's manifestRelPath() semantics.
}
```

`RelPath` is computed at walk time when the prefix is known, eliminating today's lazy `manifestRelPath()` helper at `resolver.go:591-596`.

### C. Single `RelPath` field on `ResolvedFile` and `Change`

Replaces today's `ManifestRelPath`. Set for `manifest` and `file` categories, `""` elsewhere. The applier's `applyPhaseResult` replaces the parallel `ChangedManifests` map with:

```go
type applyPhaseResult struct {
    ChangedUnits   map[string]struct{}
    ChangedSecrets map[string]struct{}
    ChangedRels    map[string]map[string]struct{}  // category → relpath set
    NeedsReload    bool
}
```

`hookMatchesChange` does one set lookup per trigger field via a small `anyIn(refs, set)` helper. Future trigger types are one map entry, not new fields in the phase-result struct. Secrets keep their own dedicated map because they key on a different identifier (`secret:<name>` not a rel path).

### D. Extract `validateRelPath`

The "clean, relative, non-escaping path" rule lives in `normalizeManifests` (`hook.go:104-117`) and `manifestPath` (`registry.go:150-160`). After this work it would land in three more spots (`normalizeFiles`, `filePath`, `bundleFileRef` construction). Pull into a shared helper — single source of truth for what counts as a clean relative path. Returns the cleaned path (callers were already shadowing the input with the cleaned value).

### E. Rename `manifestDestPath` → `dataDestPath`

Already category-agnostic — `LogicalPath` always carries the category prefix and the function is just `filepath.Join(dataDir, fromSlash(stripTmpl(logicalPath)))`. Same call site for manifest and file categories. Rename for clarity.

### What is NOT collapsed

| Surface | Reason |
|---|---|
| `Hook.Manifests`, `Hook.Files` | Public YAML schema; distinct fields keep validation messages precise |
| `manifestPath`, `filePath` template helpers | Public template API; `dataPath("manifest", "x")` would regress ergonomics |
| Per-category validator dispatch | K8s-strict for manifest, opaque-with-yaml-syntax for file |
| Hook plumbing for `manifests:` | Required for K8s reload patterns (configmap reload later, custom controllers, …) |
| Top-level `manifests:` in `assignments.yml` | Orthogonal concern; deprecation is a separate decision |

### Layer-by-layer summary

| Layer | Change |
|---|---|
| `pkg/config/assignments.go` | `Files []string` on `AssignmentGroup` and `ResolvedFileSet`; mirror in `merge`/`deduplicate` |
| `pkg/config/hook.go` | `Files []string` on `Hook`; `normalizeFiles`; the trigger-required check at `:65` (currently `len(Secrets) == 0 && len(Manifests) == 0`) extends to include `Files` |
| `pkg/resolver/services.go` | `bundleSubdirs` entry for `files/`; `manifestRef` → `bundleFileRef`; `readManifestSubdir` → `readNestedSubdir` |
| `pkg/resolver/resolver.go` | `RelPath` on `ResolvedFile`; `manifestRelPath()` removed; `manifestDestPath` → `dataDestPath`; `resolveManifestRef` becomes `resolveNestedRef(category, ref)` |
| `pkg/resolver/registry.go` | `filePath` template helper; shared `validateRelPath` |
| `pkg/validator/validator.go` | `case "file":` arm; new `validateFile` |
| `pkg/reconciler/reconciler.go` | `RelPath` on `Change` (replaces `ManifestRelPath`); the package-level `categories` slice at `:122` gains `"file"` (consumed by `Categories()` and the `pkg/agent` managed-files metric) |
| `pkg/applier/applier.go` | `categoryOrder` insert; `ChangedRels` map (replacing `ChangedManifests`); `runHooksWithPending` early-exit guard at `:391` extended to also check `len(changedRels) == 0` so `files:`-only hooks are not silently skipped; `runHooksWithPending` and `hookMatchesChange` signatures take `changedRels` instead of `changedManifests`; `hookMatchesChange` covers `Hook.Files` |
| `README.md` | Categories table; bundle layout; template helper table; hook example switched to `files: [config/scrape.yml]` and `filePath` |
| `testdata/example-fleet/` | New `files/` example under one service; refresh goldens with `-update` |

`pkg/state`, `pkg/orphan`, `pkg/agent`, `pkg/dashboard`, `pkg/metrics`, `pkg/health`, `pkg/rollback` need no behavioral changes — they consume `applier.CategoryOrder()` / `reconciler.Categories()` / `ManagedFile.Category` (free-form string), which all surface `"file"` automatically. Spot-check during implementation that no code path enumerates categories explicitly.

Mockery-generated mocks in `mocks/applier/` need no regeneration — the system-boundary interfaces (`SystemdManager`, `PodmanClient`, `FileWriter`) are unchanged.

## Testing Strategy

Picolet's table-driven coverage is the regression net. The internal genericization (A–E) is a pure refactor; every existing manifest test must stay green throughout.

**Phase 1 — baseline.** `task test` clean before touching anything.

**Phase 2 — write new-surface tests against unwritten code (red).**

- `pkg/config`: `TestHookNormalizeValidatesFiles` mirroring `TestHookNormalizeValidatesManifests` (`..`, leading `/`, empty, etc.); files-only trigger (`Hook.Files` set, `Hook.Secrets` and `Hook.Manifests` both empty) normalizes successfully; empty-trigger error case (all three empty) still fires after `Files` is added to the check.
- `pkg/resolver`: `files/` bundle expansion (flat + nested); `filePath` rootful + rootless + path validation; collision detection (`files/x` vs `manifests/x` → no collision; same logical path under `files/` from two services → collision).
- `pkg/validator`: `validateFile` truth table — plain text passes; `.yml` valid passes; `.yaml` valid passes; `.yml` invalid fails with `validateYAMLSyntax` error format; `.yml.tmpl` validates rendered output; empty file passes (deliberate); non-YAML extensions skip regardless of content.
- `pkg/reconciler`: file-category resolved file populates `Change.RelPath`.
- `pkg/applier`: `categoryOrder` contains `"file"` next to `"manifest"`; `applyPhase` populates `ChangedRels["file"][rel]`; file changes don't trigger daemon-reload; `hookMatchesChange` returns true for matching `Hook.Files`, false for unrelated changes; hook with both `manifests:` and `files:` fires on either; **a `files:`-only hook (no `secrets`, no `manifests`) actually fires** — guards against a regression of the `runHooksWithPending` early-exit at `applier.go:391` that currently short-circuits when secrets+manifests+pending are all empty.

**Phase 3 — refactor manifest path (green).** Land A–E. Existing manifest tests stay green; new file tests can stay red. Tests that referenced `ManifestRelPath` directly or `ChangedManifests` directly are rewritten to the new shape. Tests for the deleted `manifestRelPath()` helper are removed.

**Phase 4 — wire up `file` (green).** Add the `bundleSubdir` entry, the `analyzeFile` arm, the `Files` field, the `filePath` helper, the `categoryOrder` slot, the `dataDir/files/` dest path. New-surface tests turn green.

**Phase 5 — integration.** Add a `files/` directory under one bundle in `testdata/example-fleet/` with both a static file and a `.tmpl` to exercise rendering. Reference one entry from a hook in that bundle's `picolet.yml.tmpl`. Refresh goldens with `go test ... -update`. Spot-check `pkg/agent/agent_test.go` for category-enumerating assertions.

`task test` and `task lint` must be green at the end.

## Migration (iuk-gitops)

The only known consumer is `~/src/drk-darmstadt-iuk/iuk-gitops`, with three services using `manifests/` for non-K8s YAML (vmalert rules, vmalert-vlogs rules, victoriametrics scrape config).

**Repo-side migration (one atomic PR):**

1. `git mv services/<svc>/manifests services/<svc>/files` for each affected service.
2. In each `services/<svc>/picolet.yml.tmpl`: rename `manifests:` → `files:`. Path values stay the same.
3. In templates: `{{ manifestPath "x" }}` → `{{ filePath "x" }}`.
4. In `.container` `Volume=` mounts: `/var/lib/picolet/manifests/<x>` → `/var/lib/picolet/files/<x>`.
5. Verify with `grep -r 'manifests/\|manifestPath\|var/lib/picolet/manifests' services/`.

**Host-side cleanup is automatic.** When the new desired set lands:

- `state.ManagedFiles` still contains the old `/var/lib/picolet/manifests/<x>` entries.
- The resolver no longer emits those paths.
- `reconciler.Diff` classifies them as `ActionDelete` (in-state, not-in-desired).
- The applier removes them on the next reconcile tick.
- New `/var/lib/picolet/files/<x>` paths are written via `ActionCreate`.

No manual `rm` on hosts. No state migration script. No changes to `pkg/orphan` (orphan detection only governs the quadlet directory; dataDir cleanup is state-driven).

**Cross-cut ordering:**

1. Land this picolet PR; cut a release.
2. Land the iuk migration PR; CI runs `picolet validate` against the new binary and gates the merge — catches missed renames.
3. Roll the new picolet binary out to hosts via the existing deploy channel.
4. Each host migrates its on-disk state automatically on the next reconcile.

Between steps 1 and 2 (new picolet binary, old iuk repo) `picolet validate` hard-fails on each non-K8s YAML in `manifests/` with `missing 'kind' field`. This is the desired forcing function — no half-migrated state can land.

## Out of scope

- **Deprecation of top-level `manifests:` / `files:` in `assignments.yml`.** Non-bundle deployments are still supported in this design.
- **Docs hint coupling the validator error to the new category name** (e.g., "did you mean `files/`?"). The README's hook section gains the redirect prose; the validator stays decoupled.
- **K8s ConfigMap-driven container reload.** Still broken (Podman behavior, not picolet), still not picolet's responsibility. Hooks for K8s manifest changes remain wired so this can be revisited later without further design work.
- **Reusing `files/` for additional categories** (e.g., a future `scripts/` for executable mounts). Genericization (A–E) leaves room; not designing it now.

## Reporting deliverables (for the iuk follow-up PR)

- Category name: `file`
- Bundle subdir: `files/`
- Hook trigger key: `files:`
- Template helper: `filePath(relPath)`
- Deployed path: `/var/lib/picolet/files/<rel>` (rootful) / `~/.local/share/picolet/files/<rel>` (rootless)
- `assignments.yml` top-level key: `files:` (parallel to `manifests:`)
