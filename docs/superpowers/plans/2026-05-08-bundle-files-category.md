# Files Bundle Category Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the overloaded `manifests/` bundle category into K8s-strict `manifests/` and a new opaque `files/` for arbitrary container-mounted config files; fold internal genericization (unified `bundleFileRef`, single `RelPath` field, per-category `ChangedRels` map) into the same change so the new category is a thin slot, not a parallel duplication.

**Architecture:** Picolet maintains parallel slots for each bundle category (`AssignmentGroup`, `ResolvedFileSet`, `bundleSubdirs`, `categoryOrder`, validator dispatch, hook trigger fields). The hot-reload-hooks ticket (PR #71) added category-specific scaffolding (`manifestRef`, `manifestRelPath()`, `ChangedManifests`, `ManifestRelPath` field). This plan first genericizes that scaffolding so manifest+file flow through the same internal types, then adds `file` as one entry in each user-facing slot.

**Tech Stack:** Go 1.26 with build tags `remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper`. Use `task` runner — `task test`, `task lint`, `task fmt`. Mockery for test mocks (no regen needed for this work). `goldie` for golden-file integration tests. All tests use `t.Parallel()` (enforced by `tparallel` linter).

**Spec:** `docs/superpowers/specs/2026-05-08-bundle-files-category-design.md`

---

## Conventions

- Build tags: always use `task test` / `task lint`. If invoking `go test` directly, prefix with `TAGS="remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" go test -tags "$TAGS" ...`.
- All new tests use `t.Parallel()` at top-level and inside subtest closures (enforced).
- Imports are managed by `gofumpt`/`gci` — never reorder by hand. Run `task fmt` if imports look wrong.
- Commit messages follow the existing style: `feat:` / `fix:` / `refactor:` / `docs:` / `chore:` prefixes; co-author trailer is optional but match the repo's recent commits if you add one.
- Each task ends with a single commit covering only that task's changes.
- After every implementation step, run `task lint` once before committing — the linter is aggressive (cyclop, funlen, nestif, ireturn, dupl) and is much easier to fix incrementally.

## File Structure

**Created:**
- `pkg/config/relpath.go` — shared `ValidateRelPath` helper used by hooks and template helpers.

**Modified:**
- `pkg/config/assignments.go` — `Files []string` on `AssignmentGroup`, `ResolvedFileSet`; mirrored in `merge`/`deduplicate`.
- `pkg/config/hook.go` — `Files []string` on `Hook`; `normalizeFiles`; trigger-required check at `:65` extends to include `Files`; `normalizeManifests` switches to `ValidateRelPath`.
- `pkg/config/config_test.go` — `TestHookNormalizeValidatesFiles`, files-only-trigger normalization case, empty-trigger error case (all three empty).
- `pkg/resolver/services.go` — `bundleSubdirs` entry for `files/` (Category `"file"`, AllowNesting `true`); `manifestRef` → `bundleFileRef` with `Category` + `RelPath` fields; `readManifestSubdir` → `readNestedSubdir(category)`; `expandedBundles.Manifests` → `expandedBundles.NestedRefs []bundleFileRef` (carries both manifest and file refs).
- `pkg/resolver/services_test.go` — update existing `manifestRef` references to `bundleFileRef`; add file bundle expansion tests.
- `pkg/resolver/resolver.go` — `manifestDestPath` → `dataDestPath`; `manifestRelPath()` deleted (rel path stored on ref at construction); `ResolvedFile.ManifestRelPath` → `RelPath`; `resolveManifestRef` → `resolveNestedRef(ref)` (uses ref's Category).
- `pkg/resolver/resolver_test.go` — update field references.
- `pkg/resolver/registry.go` — `filePath` template helper; `manifestPath` and `filePath` both call shared `validateRelPath` from pkg/config.
- `pkg/resolver/registry_test.go` — `filePath` tests.
- `pkg/validator/validator.go` — `case "file":` arm in `analyzeFile`; new `validateFile` function.
- `pkg/validator/validator_test.go` — `validateFile` truth table.
- `pkg/reconciler/reconciler.go` — `Change.ManifestRelPath` → `RelPath`; package-level `categories` slice at `:122` gains `"file"`.
- `pkg/reconciler/reconciler_test.go` — update field references; add file-category Diff test.
- `pkg/applier/applier.go` — `"file"` in `categoryOrder` (after `"manifest"`); `applyPhaseResult.ChangedManifests` → `ChangedRels map[string]map[string]struct{}` keyed by category; `applyPhase` populates `ChangedRels[change.Category][change.RelPath]` for `manifest` and `file`; `runHooksWithPending` early-exit guard at `:391` extended to also check `len(changedRels) == 0`; `hookMatchesChange` covers `Hook.Files`.
- `pkg/applier/applier_test.go` — update `ManifestRelPath` references; add file category tests including the files-only-hook regression.
- `README.md` — categories table; bundle layout block; template helper table gains `filePath`; hot-reload example switched to `files: [config/scrape.yml]` and `filePath`.
- `testdata/example-fleet/services/web-app/files/` — new directory with one static file and one `.tmpl`; `testdata/example-fleet/services/web-app/picolet.yml` — new hooks file referencing a file trigger.
- Affected goldens under `testdata/fixtures/` — refreshed via `-update`.

---

## Phase 1 — Internal Genericization (Refactor; Existing Tests Stay Green)

These tasks change the shape of internal types but do not change behavior. After each task, the full existing test suite must remain green.

### Task 1: Baseline check

**Files:** none modified.

- [ ] **Step 1: Run the full test suite.**

```
task test
```

Expected: `ok` for every package; no `FAIL`.

- [ ] **Step 2: Run the linter.**

```
task lint
```

Expected: no findings.

- [ ] **Step 3: Confirm git is clean.**

```
git status
```

Expected: `nothing to commit, working tree clean`.

If any of these fail, fix before continuing.

---

### Task 2: Extract `ValidateRelPath` helper (spec item D)

The "clean, non-escaping relative path" rule lives in two places today (`normalizeManifests` and `manifestPath`) and will be needed in three more. Extract it to a shared helper in `pkg/config` (which has no internal dependencies, so `pkg/resolver` can import it without a cycle).

**Files:**
- Create: `pkg/config/relpath.go`
- Modify: `pkg/config/hook.go:104-117` (use the helper)
- Modify: `pkg/resolver/registry.go:150-159` (use the helper)
- Test: `pkg/config/config_test.go` (existing `TestHookNormalizeValidatesManifests` covers the rule and must stay green)

- [ ] **Step 1: Create the shared helper.**

`pkg/config/relpath.go`:
```go
package config

import (
	"errors"
	"path"
	"strings"
)

// ErrNotCleanRelPath is returned by ValidateRelPath when the input is not a
// clean, non-escaping relative path. Callers should wrap with their own
// context (e.g. "manifests[0]: %q %w") so the existing error wording is
// preserved.
var ErrNotCleanRelPath = errors.New("must be a clean relative path")

// ValidateRelPath returns the cleaned form of a relative path used to address
// a file inside a bundle category directory (e.g. manifests/, files/). It
// rejects empty strings, absolute paths, traversal segments, double slashes,
// trailing slashes, and any input that does not equal path.Clean(input).
// On success the cleaned path is returned; on failure ErrNotCleanRelPath
// is returned with no embedded path so callers can preserve their existing
// error format.
func ValidateRelPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	cleaned := path.Clean(trimmed)
	if trimmed == "" || trimmed != cleaned ||
		cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "/") ||
		strings.HasPrefix(cleaned, "../") {
		return "", ErrNotCleanRelPath
	}
	return cleaned, nil
}
```

- [ ] **Step 2: Use the helper in `normalizeManifests`.**

In `pkg/config/hook.go`, replace the body of `normalizeManifests` (currently at lines 104-117) with:

```go
func (h *Hook) normalizeManifests() error {
	for i, manifest := range h.Manifests {
		cleaned, err := ValidateRelPath(manifest)
		if err != nil {
			return fmt.Errorf("%s: manifests[%d]: %q %w", h.Name, i, manifest, err)
		}
		h.Manifests[i] = cleaned
	}
	return nil
}
```

The combined error reads `hook: manifests[0]: "X" must be a clean relative path` — byte-identical to the pre-refactor wording, so the existing `TestHookNormalizeValidatesManifests` assertions still pass.

The `path` and `strings` imports may now be unused at this site — let `task fmt` drop them if so.

- [ ] **Step 3: Use the helper in the `manifestPath` template helper.**

In `pkg/resolver/registry.go`, replace the body of the `manifestPath` func (currently at lines 150-159):

```go
"manifestPath": func(relPath string) (string, error) {
	cleaned, err := config.ValidateRelPath(relPath)
	if err != nil {
		return "", fmt.Errorf("manifestPath %q: %w", relPath, err)
	}
	return filepath.Join(dataDir, "manifests", filepath.FromSlash(cleaned)), nil
},
```

The combined error reads `manifestPath "X": must be a clean relative path` — byte-identical to the pre-refactor wording, so the existing `TestManifestPathValidatesInputs` test (`registry_test.go:163`) still passes.

If `config` is not yet imported here, add it. (`github.com/schjan/picolet/pkg/config` is used elsewhere in `pkg/resolver`; check this file specifically — the import block is at the top.)

- [ ] **Step 4: Run tests.**

```
task test
```

Expected: all green. The existing `TestHookNormalizeValidatesManifests` (covering empty, traversal, etc.) is the regression net.

- [ ] **Step 5: Run lint.**

```
task lint
```

Expected: no findings. If `funlen` or `cyclop` complain on the now-shorter `normalizeManifests`, that's actually impossible — they only complain about *long* funcs. Most likely you'll see unused-import warnings if step 2 left `path`/`strings` imports orphaned; `task fmt` fixes those.

- [ ] **Step 6: Commit.**

```
git add pkg/config/relpath.go pkg/config/hook.go pkg/resolver/registry.go
git commit -m "refactor(config): extract ValidateRelPath helper"
```

---

### Task 3: Rename `manifestDestPath` → `dataDestPath` (spec item E)

Pure rename. The function is already category-agnostic — its `logicalPath` argument always carries the category subdir prefix.

**Files:**
- Modify: `pkg/resolver/resolver.go` (rename method and all call sites)

- [ ] **Step 1: Rename the method definition.**

In `pkg/resolver/resolver.go`, change:
```go
func (r *Resolver) manifestDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(strings.TrimSuffix(logicalPath, ".tmpl")))
}
```
to:
```go
func (r *Resolver) dataDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(strings.TrimSuffix(logicalPath, ".tmpl")))
}
```

- [ ] **Step 2: Rename all call sites in the same file.**

Find each `r.manifestDestPath(` call (currently in `buildFileSkeletons` at `:300` and `resolveManifestRef` at `:582`) and change to `r.dataDestPath(`.

- [ ] **Step 3: Run tests.**

```
task test
```

Expected: all green.

- [ ] **Step 4: Commit.**

```
git add pkg/resolver/resolver.go
git commit -m "refactor(resolver): rename manifestDestPath to dataDestPath"
```

---

### Task 4: Unify `manifestRef` → `bundleFileRef` with `Category` and `RelPath` fields (spec item B)

Adds the fields the new walker needs. `RelPath` is computed at walk time so the lazy `manifestRelPath()` helper can disappear. `Category` lets a single ref slice carry both manifest and file refs.

**Files:**
- Modify: `pkg/resolver/services.go` (rename type; populate fields in walker and legacy ref constructor; rename `Manifests` slice on `expandedBundles` to `NestedRefs`)
- Modify: `pkg/resolver/resolver.go` (callers reference `ref.RelPath`/`ref.Category` instead of helper; rename uses)
- Modify: `pkg/resolver/services_test.go:37,190` (assertion struct literals change type)

- [ ] **Step 1: Replace the type definition.**

In `pkg/resolver/services.go`, replace:
```go
type manifestRef struct {
	SrcPath     string
	LogicalPath string
}
```
with:
```go
type bundleFileRef struct {
	SrcPath     string
	LogicalPath string
	Category    string // "manifest" or "file"
	RelPath     string // LogicalPath with the category subdir prefix stripped if present, otherwise unchanged
}
```

- [ ] **Step 2: Rename the slice field on `expandedBundles`.**

In the same file, change:
```go
type expandedBundles struct {
	...
	Manifests  []manifestRef
	Hooks      []hookRef
}
```
to:
```go
type expandedBundles struct {
	...
	NestedRefs []bundleFileRef
	Hooks      []hookRef
}
```

Update the sort/append blocks in `expandServiceBundles` (currently around `:99-104`) — the slice is now `expanded.NestedRefs` and the element type is `bundleFileRef`. Keep the same sort key (LogicalPath, SrcPath).

In `(b *expandedBundles).append`, change `b.Manifests = append(b.Manifests, other.Manifests...)` to `b.NestedRefs = append(b.NestedRefs, other.NestedRefs...)`.

In `(b *expandedBundles).fileCount`, change `len(b.Manifests)` to `len(b.NestedRefs)`.

- [ ] **Step 3: Populate `Category` and `RelPath` in the walker.**

In `pkg/resolver/services.go`, replace `readManifestSubdir`:

```go
func (b *expandedBundles) readManifestSubdir(fsys fs.FS, root, service string) error {
	return fs.WalkDir(fsys, root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking %s: %w", walkPath, walkErr)
		}
		if walkPath == root || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: expected regular file", walkPath)
		}
		logical := stripServicePrefix(walkPath, service)
		b.NestedRefs = append(b.NestedRefs, bundleFileRef{
			SrcPath:     walkPath,
			LogicalPath: logical,
			Category:    "manifest",
			RelPath:     stripCategoryPrefix(logical, "manifests"),
		})
		return nil
	})
}
```

Add the helper at the bottom of the same file:
```go
// stripCategoryPrefix strips the leading "<category>/" segment from a logical
// path (e.g. "manifests/app/foo.yml" -> "app/foo.yml" for category "manifests").
// Logical paths from legacy top-level assignments may not start with the category
// dir; in that case the input is returned unchanged.
func stripCategoryPrefix(logical, categorySubdir string) string {
	if rel, ok := strings.CutPrefix(logical, categorySubdir+"/"); ok {
		return rel
	}
	return logical
}
```

(`strings` is already imported.)

Note: this task does not yet rename the function — the walker is generalized in Task 5.

- [ ] **Step 4: Update `newLegacyManifestRef` and the legacy-merge code in `resolver.go`.**

In `pkg/resolver/resolver.go`, replace:
```go
func newLegacyManifestRef(srcPath string) manifestRef {
	return manifestRef{SrcPath: srcPath, LogicalPath: srcPath}
}
```
with:
```go
func newLegacyManifestRef(srcPath string) bundleFileRef {
	return bundleFileRef{
		SrcPath:     srcPath,
		LogicalPath: srcPath,
		Category:    "manifest",
		RelPath:     stripCategoryPrefix(srcPath, "manifests"),
	}
}
```

In `expandFileSet`, replace the variable name and slice type:
```go
manifestRefs := make([]bundleFileRef, 0, len(fileSet.Manifests)+len(expanded.NestedRefs))
for _, srcPath := range fileSet.Manifests {
	manifestRefs = append(manifestRefs, newLegacyManifestRef(srcPath))
}
manifestRefs = append(manifestRefs, expanded.NestedRefs...)
return merged, uniqueManifestRefs(manifestRefs), expanded.Hooks, nil
```

Update the signature of `expandFileSet`, `expandedResult.ManifestRefs`, `uniqueManifestRefs`, `buildFileSkeletons`, `buildFiles`, `resolveManifestRef`, and `collectOpTemplateRefs` — every spot that takes `[]manifestRef` becomes `[]bundleFileRef`. The compiler will tell you exactly where.

- [ ] **Step 5: Replace `manifestRelPath(...)` calls with `ref.RelPath`.**

In `pkg/resolver/resolver.go`:

- `buildFileSkeletons` (around `:298-303`): replace `ManifestRelPath: manifestRelPath(ref.LogicalPath)` with `ManifestRelPath: ref.RelPath` (the field rename comes in Task 6).
- `resolveManifestRef` (around `:580-586`): replace `ManifestRelPath: manifestRelPath(ref.LogicalPath)` with `ManifestRelPath: ref.RelPath`.

- [ ] **Step 6: Delete the now-unused `manifestRelPath` helper.**

In `pkg/resolver/resolver.go`, delete the function at `:591-596`:
```go
func manifestRelPath(logicalPath string) string { ... }
```

- [ ] **Step 7: Update existing tests that name `manifestRef`.**

`pkg/resolver/services_test.go` has two assertion blocks at `:37` and `:190` that use `[]manifestRef{...}`. Change both to `[]bundleFileRef{...}` and add the new fields:

```go
assert.Equal(t, []bundleFileRef{
	{
		SrcPath:     "services/web/manifests/app/configs/app.conf",
		LogicalPath: "manifests/app/configs/app.conf",
		Category:    "manifest",
		RelPath:     "app/configs/app.conf",
	},
	{
		SrcPath:     "services/web/manifests/app/deployment.yml.tmpl",
		LogicalPath: "manifests/app/deployment.yml.tmpl",
		Category:    "manifest",
		RelPath:     "app/deployment.yml.tmpl",
	},
}, expanded.NestedRefs)
```

(The `expanded.Manifests` access is also renamed to `expanded.NestedRefs`.)

- [ ] **Step 8: Run tests.**

```
task test
```

Expected: all green. The compiler will catch any spots you missed in steps 4-5; fix them.

- [ ] **Step 9: Run lint and commit.**

```
task lint && git add -A && git commit -m "refactor(resolver): unify manifestRef into bundleFileRef with Category and RelPath"
```

---

### Task 5: Generalize `readManifestSubdir` → `readNestedSubdir(category)` (spec item A)

Now that the walker stamps `Category` and `RelPath`, parametrize the function so the same code can be called for any nested-allowed bundle subdir.

**Files:**
- Modify: `pkg/resolver/services.go`

- [ ] **Step 1: Take the `bundleSubdir` struct directly in the walker.**

Replace the function from Task 4 step 3 with:
```go
func (b *expandedBundles) readNestedSubdir(fsys fs.FS, root, service string, subdir bundleSubdir) error {
	return fs.WalkDir(fsys, root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking %s: %w", walkPath, walkErr)
		}
		if walkPath == root || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: expected regular file", walkPath)
		}
		logical := stripServicePrefix(walkPath, service)
		b.NestedRefs = append(b.NestedRefs, bundleFileRef{
			SrcPath:     walkPath,
			LogicalPath: logical,
			Category:    subdir.Category,
			RelPath:     stripCategoryPrefix(logical, subdir.Subdir),
		})
		return nil
	})
}
```

Caller already has the `bundleSubdir` value — passing the struct avoids a category→subdir lookup and the panic helper that lookup would need.

- [ ] **Step 2: Update the dispatch in `readSubdir`.**

Replace the body of `(b *expandedBundles).readSubdir` (currently at `:262-268`):
```go
func (b *expandedBundles) readSubdir(fsys fs.FS, service string, subdir bundleSubdir) error {
	root := path.Join("services", service, subdir.Subdir)
	if subdir.AllowNesting {
		return b.readNestedSubdir(fsys, root, service, subdir)
	}
	return b.readFlatSubdir(fsys, root, subdir.Category)
}
```

- [ ] **Step 3: Run tests.**

```
task test
```

Expected: all green.

- [ ] **Step 4: Run lint and commit.**

```
task lint && git add -A && git commit -m "refactor(resolver): generalize readManifestSubdir to readNestedSubdir(category)"
```

---

### Task 6: Rename `ResolvedFile.ManifestRelPath` → `RelPath` (spec item C-1)

Renames the field on `ResolvedFile` so manifest and file categories share it. No semantic change.

**Files:**
- Modify: `pkg/resolver/resolver.go` (field definition + populate sites)
- Modify: `pkg/resolver/resolver_test.go` (struct literals naming the field)
- Modify: any other callers that read the field (compiler will surface them)

- [ ] **Step 1: Rename the struct field.**

In `pkg/resolver/resolver.go`, in the `ResolvedFile` struct (around `:33-49`), change:
```go
	// ManifestRelPath is the manifest path relative to the manifests/ directory
	// (e.g. "config/scrape.yml"). Only set for manifest-category files.
	ManifestRelPath string
```
to:
```go
	// RelPath is the file's path relative to its bundle category directory
	// (e.g. "config/scrape.yml" for a manifest at manifests/config/scrape.yml).
	// Only set for manifest- and file-category resolved files.
	RelPath string
```

- [ ] **Step 2: Update populate sites in the same file.**

In `buildFileSkeletons` (around `:298-303`):
```go
skeletons = append(skeletons, ResolvedFile{
	SrcPath: ref.SrcPath, Category: ref.Category, DestPath: r.dataDestPath(ref.LogicalPath),
	RelPath: ref.RelPath,
})
```

In `resolveManifestRef` — rename it now to `resolveNestedRef` and use the ref's category:
```go
func (r *Resolver) resolveNestedRef(registry *template.Template, tmplData *TemplateData, ref bundleFileRef) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
	}
	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: r.dataDestPath(ref.LogicalPath),
		Content:  content,
		Category: ref.Category,
		RelPath:  ref.RelPath,
	}, nil
}
```

In `buildFiles` (around `:369-374`), change the call:
```go
for _, ref := range manifestRefs {
	f, err := r.resolveNestedRef(registry, tmplData, ref)
	if err != nil {
		return nil, err
	}
	files = append(files, *f)
}
```

- [ ] **Step 3: Update only the readers of `ResolvedFile.ManifestRelPath`.**

Task 6 renames the field on **`ResolvedFile`** only; `Change.ManifestRelPath` is renamed in Task 7. So this step touches only the spots that read a `ResolvedFile`'s field.

The compile-required updates:

- `pkg/reconciler/reconciler.go` `classifyFile` (around `:78-100`): the right-hand side `f.ManifestRelPath` → `f.RelPath`. The left-hand side `c.ManifestRelPath` (a `Change` field) **stays** for now.
  ```go
  c := Change{
      ...
      ManifestRelPath: f.RelPath, // RHS renamed; LHS still old until Task 7
  }
  ```
- `pkg/resolver/resolver_test.go`: any `ResolvedFile{... ManifestRelPath: ...}` literal becomes `ResolvedFile{... RelPath: ...}`. Run:
  ```
  grep -n 'ResolvedFile{' pkg/resolver/resolver_test.go
  ```
  to enumerate candidates.

The `Change` literal at `pkg/applier/applier_test.go:1016` keeps `ManifestRelPath: "..."` for now — it's a `Change`, not a `ResolvedFile`. Task 7 renames it.

After this step, `grep -rn 'ManifestRelPath' --include='*.go' .` should still return matches — they're all `Change`-related and will be cleared in Task 7.

- [ ] **Step 4: Run tests.**

```
task test
```

Expected: all green. Note `pkg/applier/applier_test.go:1016` still uses `ManifestRelPath` for the `reconciler.Change` field — that field is renamed in Task 7. For now, the resolver field is `RelPath` and the change field is still `ManifestRelPath`. They compile independently.

- [ ] **Step 5: Run lint and commit.**

```
task lint && git add -A && git commit -m "refactor(resolver): rename ResolvedFile.ManifestRelPath to RelPath"
```

---

### Task 7: Rename `Change.ManifestRelPath` → `RelPath` (spec item C-2)

**Files:**
- Modify: `pkg/reconciler/reconciler.go` (field on `Change`; populate site in `classifyFile`)
- Modify: `pkg/reconciler/reconciler_test.go` (struct literals)
- Modify: `pkg/applier/applier.go` (read sites)
- Modify: `pkg/applier/applier_test.go:1016` (struct literal in test fixture)

- [ ] **Step 1: Rename the field on `Change`.**

In `pkg/reconciler/reconciler.go`, in the `Change` struct (around `:23-32`):
```go
	RelPath         string // relative to category dir; "" for non-manifest/file changes
```
(replacing `ManifestRelPath`).

- [ ] **Step 2: Update `classifyFile` to populate the new name.**

In the same file, change:
```go
	c := Change{
		DestPath: f.DestPath,
		Category: f.Category,
		OldHash:  mf.Hash,
		NewHash:  newHash,
		ServiceName: f.ServiceName,
		RelPath:     f.RelPath,
	}
```

- [ ] **Step 3: Update applier read sites.**

In `pkg/applier/applier.go`, around `:289`:
```go
if change.Category == "manifest" && change.RelPath != "" {
	if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
		p.ChangedManifests[change.RelPath] = struct{}{}
	}
}
```

(The `ChangedManifests` map name is updated in Task 8; just the field rename here.)

- [ ] **Step 4: Update tests.**

`pkg/applier/applier_test.go:1016`:
```go
{DestPath: "/var/lib/picolet/manifests/config/scrape.yml", Category: "manifest", Action: reconciler.ActionUpdate, NewContent: "x", RelPath: "config/scrape.yml"},
```

`grep -rn 'ManifestRelPath' --include='*.go' pkg/` — should now return zero hits.

- [ ] **Step 5: Run tests, lint, commit.**

```
task test && task lint && git add -A && git commit -m "refactor(reconciler): rename Change.ManifestRelPath to RelPath"
```

---

### Task 8: Replace `ChangedManifests` → `ChangedRels`; fix early-exit guard (spec item C-3)

Per-category map keyed by category name. Future trigger types are one map entry, not new struct fields. **Critically**, the early-exit at `applier.go:391` must be extended so `files:`-only hooks (or any future trigger type) are not silently skipped.

**Files:**
- Modify: `pkg/applier/applier.go`
- Modify: `pkg/applier/applier_test.go` (any test that names `ChangedManifests` directly)

- [ ] **Step 1: Update `applyPhaseResult`.**

Around `:200-206` in `pkg/applier/applier.go`:
```go
type applyPhaseResult struct {
	ChangedUnits   map[string]struct{}
	ChangedSecrets map[string]struct{}
	ChangedRels    map[string]map[string]struct{} // category → relpath set
	NeedsReload    bool
}
```

Initialize in `applyPhase` (around `:249-254`):
```go
p := &applyPhaseResult{
	ChangedUnits:   make(map[string]struct{}),
	ChangedSecrets: make(map[string]struct{}),
	ChangedRels:    make(map[string]map[string]struct{}),
}
```

- [ ] **Step 2: Populate `ChangedRels` for manifest changes.**

Replace the block at `:289-293`:
```go
if (change.Category == "manifest" || change.Category == "file") && change.RelPath != "" {
	if change.Action == reconciler.ActionCreate || change.Action == reconciler.ActionUpdate {
		if p.ChangedRels[change.Category] == nil {
			p.ChangedRels[change.Category] = make(map[string]struct{})
		}
		p.ChangedRels[change.Category][change.RelPath] = struct{}{}
	}
}
```

This handles both manifest and (after Task 11) file changes.

- [ ] **Step 3: Update `Apply`/`ApplyWithPending` call site.**

In `ApplyWithPending` (around `:235`):
```go
hookRestartUnits := a.runHooksWithPending(ctx, phase.ChangedSecrets, phase.ChangedRels, phase.ChangedUnits, pendingNames, result)
```

- [ ] **Step 4: Update `runHooksWithPending` signature and early-exit guard.**

Around `:385-395`:
```go
func (a *Applier) runHooksWithPending(
	ctx context.Context,
	changedSecrets map[string]struct{},
	changedRels map[string]map[string]struct{},
	restartScheduled map[string]struct{},
	pendingNames []string,
	result *ApplyResult,
) map[string]struct{} {
	if len(changedSecrets) == 0 && len(changedRels) == 0 && len(pendingNames) == 0 {
		return nil
	}
	...
```

The `len(changedRels) == 0` test — note that `changedRels` is `map[string]map[string]struct{}`; an empty outer map (no categories changed) is `len() == 0`. We do NOT want to add a separate sum-over-inner-maps check; if any inner map exists with at least one entry, the outer length is ≥ 1. (Verify by reading the populate site: we only insert an inner map when we have at least one rel to record.)

- [ ] **Step 5: Update `hookMatchesChange` signature.**

Around `:528-540`:
```go
func hookMatchesChange(hook config.Hook, changedSecrets map[string]struct{}, changedRels map[string]map[string]struct{}) bool {
	for _, secret := range hook.Secrets {
		if _, ok := changedSecrets[secret]; ok {
			return true
		}
	}
	for _, manifest := range hook.Manifests {
		if _, ok := changedRels["manifest"][manifest]; ok {
			return true
		}
	}
	return false
}
```

(`Hook.Files` will be added to this function in Task 11. `changedRels["manifest"]` returns `nil` when no manifests changed; map lookup on `nil` returns zero value, so this is safe.)

- [ ] **Step 6: Update `runOneHook` and `runHooksWithPending` body.**

Inside `runHooksWithPending`, replace `hookMatchesChange(hook, changedSecrets, changedManifests)` with `hookMatchesChange(hook, changedSecrets, changedRels)` (around `:405`).

- [ ] **Step 7: Update `RunPendingHooks`.**

Around `:493`, the call passes `nil` for the manifest map; pass `nil` for rels too:
```go
restartUnits := a.runHooksWithPending(ctx, nil, nil, nil, pendingNames, result)
```

- [ ] **Step 8: Update tests.**

`grep -rn 'ChangedManifests' --include='*.go' pkg/applier/` — replace any direct uses. (Most tests interact through the public `Apply` entry point, so this is usually a one- or two-line change.)

- [ ] **Step 9: Run tests.**

```
task test
```

Expected: all green. The existing `TestSecretHook*` tests (around `:1000`) verify that a manifest-trigger hook fires when a manifest changes — these regression-test the new map-of-maps shape end-to-end.

- [ ] **Step 10: Run lint and commit.**

```
task lint && git add -A && git commit -m "refactor(applier): replace ChangedManifests with per-category ChangedRels and fix early-exit guard"
```

---

## Phase 2 — Add the `file` Category (TDD: tests first)

After Phase 1, the internal types support multiple nested-allowed categories. Phase 2 adds the new `file` category one slot at a time.

### Task 9: `Files` field in `AssignmentGroup` and `ResolvedFileSet`

**Files:**
- Modify: `pkg/config/assignments.go`
- Modify: `pkg/config/config_test.go` (add a small test for files-merging)

- [ ] **Step 1: Write a failing test.**

Append to `pkg/config/config_test.go`:
```go
func TestAssignmentsResolveMergesFiles(t *testing.T) {
	t.Parallel()
	a := &Assignments{
		Base: AssignmentGroup{Files: []string{"shared/base.yml"}},
		Features: map[string]AssignmentGroup{
			"observability": {Files: []string{"shared/obs.yml"}},
		},
	}
	host := &HostConfig{Hostname: "h", PiType: "p", Features: []string{"observability"}}
	resolved := a.Resolve(host)
	assert.Equal(t, []string{"shared/base.yml", "shared/obs.yml"}, resolved.Files)
}
```

- [ ] **Step 2: Run the test.**

```
task test -- -run TestAssignmentsResolveMergesFiles ./pkg/config/
```

Expected: compile error — `unknown field Files in struct literal of type AssignmentGroup`.

- [ ] **Step 3: Add the `Files` field.**

In `pkg/config/assignments.go`:
- `AssignmentGroup` (around `:16-25`): add `Files []string \`yaml:"files"\`` after `Manifests`.
- `ResolvedFileSet` (around `:28-37`): add `Files []string` after `Manifests`.
- `(r *ResolvedFileSet).deduplicate` (around `:60-69`): add `r.Files = sortedUnique(r.Files)`.
- `(r *ResolvedFileSet).merge` (around `:76-85`): add `r.Files = append(r.Files, g.Files...)`.

- [ ] **Step 4: Run tests.**

```
task test
```

Expected: all green.

- [ ] **Step 5: Commit.**

```
task lint && git add -A && git commit -m "feat(config): add Files field to AssignmentGroup and ResolvedFileSet"
```

---

### Task 10: `Hook.Files` field, `normalizeFiles`, trigger-required check

**Files:**
- Modify: `pkg/config/hook.go`
- Modify: `pkg/config/config_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `pkg/config/config_test.go`:
```go
func TestHookNormalizeValidatesFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{name: "simple file", file: "scrape.yml"},
		{name: "nested file", file: "config/scrape.yml"},
		{name: "absolute path rejected", file: "/etc/passwd", wantErr: "must be a clean relative path"},
		{name: "traversal rejected", file: "../etc/passwd", wantErr: "must be a clean relative path"},
		{name: "embedded traversal rejected", file: "a/../b", wantErr: "must be a clean relative path"},
		{name: "double slash rejected", file: "a//b.yml", wantErr: "must be a clean relative path"},
		{name: "dot rejected", file: ".", wantErr: "must be a clean relative path"},
		{name: "empty rejected", file: "", wantErr: "must be a clean relative path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := Hook{
				Name:   "hook",
				Files:  []string{tt.file},
				Unit:   "app.service",
				Action: HookActionRestart,
			}
			err := h.Normalize()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestHookNormalizeRequiresAtLeastOneTrigger(t *testing.T) {
	t.Parallel()
	h := Hook{Name: "hook", Unit: "app.service", Action: HookActionRestart}
	err := h.Normalize()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of secrets, manifests, or files is required")
}

func TestHookNormalizeFilesOnlyTrigger(t *testing.T) {
	t.Parallel()
	h := Hook{
		Name:   "hook",
		Files:  []string{"config/foo.yml"},
		Unit:   "app.service",
		Action: HookActionRestart,
	}
	require.NoError(t, h.Normalize())
}
```

- [ ] **Step 2: Run tests; expect compile error.**

```
task test -- -run TestHookNormalizeValidatesFiles ./pkg/config/
```

Expected: `Hook` has no field `Files`.

- [ ] **Step 3: Add `Hook.Files` and `normalizeFiles`.**

In `pkg/config/hook.go`:
- `Hook` struct (around `:26-39`): add after `Manifests`:
```go
	Files []string `yaml:"files"`
```
- After `normalizeManifests`, add:
```go
func (h *Hook) normalizeFiles() error {
	for i, file := range h.Files {
		cleaned, err := ValidateRelPath(file)
		if err != nil {
			return fmt.Errorf("%s: files[%d]: %w", h.Name, i, err)
		}
		h.Files[i] = cleaned
	}
	return nil
}
```
- `Normalize` (around `:65`): change the trigger-required check to:
```go
if len(h.Secrets) == 0 && len(h.Manifests) == 0 && len(h.Files) == 0 {
	return fmt.Errorf("%s: at least one of secrets, manifests, or files is required", h.Name)
}
```
- In the same `Normalize` function, after the `normalizeManifests` call, add:
```go
if err := h.normalizeFiles(); err != nil {
	return err
}
```

- [ ] **Step 4: Run tests.**

```
task test
```

Expected: all green.

- [ ] **Step 5: Commit.**

```
task lint && git add -A && git commit -m "feat(config): add Hook.Files trigger field"
```

---

### Task 11: `bundleSubdirs` entry for `files/`; `Files` legacy ref support

`hookMatchesChange` is wired in Task 15 alongside its regression test — they read together.

**Files:**
- Modify: `pkg/resolver/services.go` (bundleSubdirs entry)
- Modify: `pkg/resolver/resolver.go` (legacy file refs from `fileSet.Files`)
- Modify: `pkg/resolver/services_test.go` (new test for files/ bundle)

- [ ] **Step 1: Write failing test for files/ bundle expansion.**

Append to `pkg/resolver/services_test.go` a new test mirroring the existing manifest test in spirit:
```go
func TestExpandServiceBundlesIncludesFilesCategory(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers/web.container":              &fstest.MapFile{Data: []byte("c")},
		"services/web/files/scrape.yml":                      &fstest.MapFile{Data: []byte("a: 1")},
		"services/web/files/rules/alerts.yml":                &fstest.MapFile{Data: []byte("b: 2")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)
	require.Len(t, expanded.NestedRefs, 2)
	assert.Equal(t, bundleFileRef{
		SrcPath:     "services/web/files/rules/alerts.yml",
		LogicalPath: "files/rules/alerts.yml",
		Category:    "file",
		RelPath:     "rules/alerts.yml",
	}, expanded.NestedRefs[0])
	assert.Equal(t, bundleFileRef{
		SrcPath:     "services/web/files/scrape.yml",
		LogicalPath: "files/scrape.yml",
		Category:    "file",
		RelPath:     "scrape.yml",
	}, expanded.NestedRefs[1])
}
```

- [ ] **Step 2: Run; expect failure (files/ not registered).**

```
task test -- -run TestExpandServiceBundlesIncludesFilesCategory ./pkg/resolver/
```

Expected: error `services/web/files: unknown entry`.

- [ ] **Step 3: Add the bundleSubdirs entry.**

In `pkg/resolver/services.go`, add to the `bundleSubdirs` slice (around `:19-27`):
```go
var bundleSubdirs = []bundleSubdir{
	{Subdir: "containers", Category: "container", AllowNesting: false},
	{Subdir: "volumes", Category: "volume", AllowNesting: false},
	{Subdir: "networks", Category: "network", AllowNesting: false},
	{Subdir: "kube", Category: "kube", AllowNesting: false},
	{Subdir: "systemd", Category: "systemd", AllowNesting: false},
	{Subdir: "secrets", Category: "secret", AllowNesting: false},
	{Subdir: "manifests", Category: "manifest", AllowNesting: true},
	{Subdir: "files", Category: "file", AllowNesting: true},
}
```

- [ ] **Step 4: Run; expect green.**

```
task test -- -run TestExpandServiceBundlesIncludesFilesCategory ./pkg/resolver/
```

Expected: pass.

- [ ] **Step 5: Add legacy-file-ref support in expandFileSet.**

In `pkg/resolver/resolver.go`, in `expandFileSet`, after the existing manifest-legacy loop (around `:255-258`), add:
```go
for _, srcPath := range fileSet.Files {
	manifestRefs = append(manifestRefs, bundleFileRef{
		SrcPath:     srcPath,
		LogicalPath: srcPath,
		Category:    "file",
		RelPath:     stripCategoryPrefix(srcPath, "files"),
	})
}
```

- [ ] **Step 6: Run all tests, lint, commit.**

```
task test && task lint && git add -A && git commit -m "feat(resolver): wire files/ category through bundle expansion"
```

---

### Task 12: `filePath` template helper

**Files:**
- Modify: `pkg/resolver/registry.go`
- Modify: `pkg/resolver/registry_test.go`

- [ ] **Step 1: Write failing tests.**

Reuse the existing `renderRegistryTemplate` helper at `registry_test.go:13` — same style as `TestManifestPathValidatesInputs` at `:163`. Append to `pkg/resolver/registry_test.go`:

```go
func TestFilePathValidatesInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "simple file", input: "scrape.yml", want: "/var/lib/picolet/files/scrape.yml"},
		{name: "nested file", input: "config/scrape.yml", want: "/var/lib/picolet/files/config/scrape.yml"},
		{name: "absolute path rejected", input: "/etc/passwd", wantErr: "must be a clean relative path"},
		{name: "traversal segment rejected", input: "../etc/passwd", wantErr: "must be a clean relative path"},
		{name: "embedded traversal rejected", input: "a/../b", wantErr: "must be a clean relative path"},
		{name: "double slash rejected", input: "a//b.yml", wantErr: "must be a clean relative path"},
		{name: "trailing slash rejected", input: "a/", wantErr: "must be a clean relative path"},
		{name: "dot rejected", input: ".", wantErr: "must be a clean relative path"},
		{name: "empty rejected", input: "", wantErr: "must be a clean relative path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				"main.tmpl": &fstest.MapFile{Data: []byte(`{{ filePath "` + tt.input + `" }}`)},
			}
			out, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}
```

- [ ] **Step 2: Run; expect failure (filePath undefined).**

```
task test -- -run TestFilePathHelper ./pkg/resolver/
```

Expected: template parse error `function "filePath" not defined`.

- [ ] **Step 3: Implement the helper.**

In `pkg/resolver/registry.go`, after the `manifestPath` entry in the `funcMap` (around `:160`):
```go
"filePath": func(relPath string) (string, error) {
	cleaned, err := config.ValidateRelPath(relPath)
	if err != nil {
		return "", fmt.Errorf("filePath %w", err)
	}
	return filepath.Join(dataDir, "files", filepath.FromSlash(cleaned)), nil
},
```

- [ ] **Step 4: Run, lint, commit.**

```
task test && task lint && git add -A && git commit -m "feat(resolver): add filePath template helper"
```

---

### Task 13: `validateFile` + `analyzeFile` `case "file":` arm

**Files:**
- Modify: `pkg/validator/validator.go`
- Modify: `pkg/validator/validator_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `pkg/validator/validator_test.go`:
```go
func TestValidateFileTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		srcPath string
		content string
		wantErr string
	}{
		{name: "plain text passes", srcPath: "files/notes.txt", content: "hello"},
		{name: "yml valid passes", srcPath: "files/scrape.yml", content: "scrape_configs:\n  - job_name: x\n"},
		{name: "yaml valid passes", srcPath: "files/scrape.yaml", content: "scrape_configs: []\n"},
		{name: "yml invalid fails", srcPath: "files/bad.yml", content: "scrape_configs: [\n - broken\n", wantErr: "YAML parse error"},
		{name: "yml.tmpl validates rendered", srcPath: "files/x.yml.tmpl", content: ":\n  - bad\n", wantErr: "YAML parse error"},
		{name: "empty file passes", srcPath: "files/empty.yml", content: ""},
		{name: "non-yaml extension skips validation", srcPath: "files/raw.bin", content: "\x00\x01not yaml\x02"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := resolver.ResolvedFile{
				SrcPath:  tt.srcPath,
				DestPath: "/var/lib/picolet/" + tt.srcPath,
				Content:  tt.content,
				Category: "file",
			}
			_, err := AnalyzeFiles([]resolver.ResolvedFile{f}, false)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
```

- [ ] **Step 2: Run; expect failure.**

```
task test -- -run TestValidateFileTruthTable ./pkg/validator/
```

Expected: error `unknown file category "file"`.

- [ ] **Step 3: Implement the validator and dispatch arm.**

In `pkg/validator/validator.go`:

Add `case "file":` to the `analyzeFile` switch (around `:80-101`):
```go
case "file":
	return status.UnitDependencies{}, validateFile(f)
```

Add `validateFile` at the bottom of the file:
```go
// validateFile checks opaque container-mounted files. Empty content is allowed
// (legitimate for empty allowlists/rule sets). Files whose source extension is
// .yml or .yaml (after stripping .tmpl) are syntax-checked; anything else is
// considered opaque and not inspected.
func validateFile(f resolver.ResolvedFile) error {
	effectivePath := strings.TrimSuffix(strings.ToLower(f.SrcPath), ".tmpl")
	switch filepath.Ext(effectivePath) {
	case ".yml", ".yaml":
		return validateYAMLSyntax(f.DestPath, []byte(f.Content))
	default:
		return nil
	}
}
```

- [ ] **Step 4: Run, lint, commit.**

```
task test && task lint && git add -A && git commit -m "feat(validator): validate files/ category with opaque YAML-syntax check"
```

---

### Task 14: `"file"` in `categoryOrder`

**Files:**
- Modify: `pkg/applier/applier.go`
- Modify: `pkg/applier/applier_test.go` (assert presence; add one applyPhase test)

- [ ] **Step 1: Write a failing test.**

Append to `pkg/applier/applier_test.go`:
```go
func TestCategoryOrderIncludesFileNextToManifest(t *testing.T) {
	t.Parallel()
	order := applier.CategoryOrder()
	manifestIdx := slices.Index(order, "manifest")
	fileIdx := slices.Index(order, "file")
	require.NotEqual(t, -1, manifestIdx, "manifest must be present")
	require.NotEqual(t, -1, fileIdx, "file must be present")
	assert.Equal(t, manifestIdx+1, fileIdx, "file must come immediately after manifest")
}
```

(Add `slices` to the imports if not already present.)

- [ ] **Step 2: Run; expect failure.**

```
task test -- -run TestCategoryOrderIncludesFileNextToManifest ./pkg/applier/
```

Expected: `file must be present`.

- [ ] **Step 3: Insert into `categoryOrder`.**

In `pkg/applier/applier.go`, in the `categoryOrder` slice (around `:137-145`):
```go
var categoryOrder = []string{
	"network",
	"volume",
	"secret",
	"systemd",
	"manifest",
	"file",
	"container",
	"kube",
}
```

- [ ] **Step 4: Run, lint, commit.**

```
task test && task lint && git add -A && git commit -m "feat(applier): add file category to categoryOrder"
```

---

### Task 15: `"file"` in `reconciler.categories`

**Files:**
- Modify: `pkg/reconciler/reconciler.go`
- Modify: `pkg/reconciler/reconciler_test.go`

- [ ] **Step 1: Write a failing test.**

Append to `pkg/reconciler/reconciler_test.go`:
```go
func TestCategoriesIncludesFile(t *testing.T) {
	t.Parallel()
	assert.Contains(t, Categories(), "file")
}
```

- [ ] **Step 2: Run; expect failure.**

```
task test -- -run TestCategoriesIncludesFile ./pkg/reconciler/
```

Expected: assertion failure.

- [ ] **Step 3: Add to the slice.**

In `pkg/reconciler/reconciler.go` at `:122`:
```go
var categories = []string{"container", "network", "volume", "kube", "systemd", "manifest", "file", "secret"}
```

- [ ] **Step 4: Wire `Hook.Files` into `hookMatchesChange`.**

In `pkg/applier/applier.go` (around `:528`), extend `hookMatchesChange`:
```go
func hookMatchesChange(hook config.Hook, changedSecrets map[string]struct{}, changedRels map[string]map[string]struct{}) bool {
	for _, secret := range hook.Secrets {
		if _, ok := changedSecrets[secret]; ok {
			return true
		}
	}
	for _, manifest := range hook.Manifests {
		if _, ok := changedRels["manifest"][manifest]; ok {
			return true
		}
	}
	for _, file := range hook.Files {
		if _, ok := changedRels["file"][file]; ok {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Add a regression test for files-only hook firing.**

This is the issue the spec calls out from review feedback. Append to `pkg/applier/applier_test.go`:

```go
func TestApplyFiresFilesOnlyHook(t *testing.T) {
	t.Parallel()

	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().GetUnitStatus(mock.Anything, "app.service").Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	pod := appliermocks.NewMockPodmanClient(t)
	fw := newMemFileWriter()

	var reloads atomic.Int32
	client := testHTTPClient(func(_ *http.Request) int {
		reloads.Add(1)
		return http.StatusOK
	})

	hooks := []config.Hook{{
		Name:      "scrape-reload",
		Files:     []string{"config/scrape.yml"},
		Unit:      "app.service",
		Action:    config.HookActionHTTP,
		Method:    http.MethodPost,
		URL:       "http://example.test/reload",
		OnFailure: config.HookOnFailureKeepRunning,
	}}
	reloader := applier.NewHookReloader(sys, pod).WithHTTPClient(client).WithHealthDelay(0)
	a := applier.New(sys, pod, fw, false, hooks, applier.WithHookReloader(reloader))

	result, err := a.Apply(t.Context(), &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/var/lib/picolet/files/config/scrape.yml", Category: "file", Action: reconciler.ActionUpdate, NewContent: "x", RelPath: "config/scrape.yml"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), reloads.Load(), "files-only hook must fire (regression: applier.go:391 early-exit)")
	assert.ElementsMatch(t, []string{"scrape-reload"}, result.AttemptedHookNames)
}
```

(Match the helper-naming style of the existing `TestSecretHookReloaderFires*` test in this file. Imports `atomic`, `http`, `mock`, etc. follow that test's pattern.)

- [ ] **Step 6: Run, lint, commit.**

```
task test && task lint && git add -A && git commit -m "feat(applier,reconciler): wire Hook.Files matching and regression test"
```

---

## Phase 3 — Integration & Documentation

### Task 16: Add `files/` example to `testdata/example-fleet/`

**Files:**
- Create: `testdata/example-fleet/services/web-app/files/scrape.yml.tmpl`
- Create: `testdata/example-fleet/services/web-app/picolet.yml`
- Modify: `testdata/example-fleet/assignments.yml` (if it currently lists web-app's manifest explicitly, leave alone — bundle handles files automatically)

- [ ] **Step 1: Add a templated file under `files/`.**

`testdata/example-fleet/services/web-app/files/scrape.yml.tmpl`:
```yaml
scrape_configs:
  - job_name: web-app
    static_configs:
      - targets: ["{{ .Host.Hostname }}:{{ index .Ports "web-app" }}"]
```

- [ ] **Step 2: Add a hook that triggers on the file.**

Create as `picolet.yml.tmpl` (not `picolet.yml`) so the URL template expression is rendered. This also exercises the hook-template code path.

`testdata/example-fleet/services/web-app/picolet.yml.tmpl`:
```yaml
hooks:
  - name: web-app-scrape-reload
    files: [scrape.yml]
    unit: web-app.container
    action: http
    method: GET
    url: 'http://localhost:{{ index .Ports "web-app" }}/-/reload'
```

- [ ] **Step 3: Run integration tests with `-update`.**

```
TAGS="remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" go test -tags "$TAGS" ./... -update
```

This refreshes any `goldie` golden fixtures touched by the new files entry.

- [ ] **Step 4: Inspect and stage golden changes.**

```
git status testdata/
git diff testdata/
```

Expect: new golden files for the `files/` deploy paths and possibly an updated host fixture. Verify each diff is what you expect (new file path under `/var/lib/picolet/files/...`, hook entry visible).

- [ ] **Step 5: Run tests without `-update` to confirm goldens match.**

```
task test
```

Expected: all green.

- [ ] **Step 6: Commit.**

```
task lint && git add -A && git commit -m "test(integration): add files/ category to example fleet"
```

---

### Task 17: Update README

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the categories table.**

In `README.md` around `:155-164`, add a row for `files/` after the `manifests/` row:
```
| `files/<app>/` | any | `/var/lib/picolet/files/<app>/` |
```

Also tighten the `manifests/` row's "Extension" column to clarify the K8s-only intent:
```
| `manifests/<app>/` | `.yml` (Kubernetes resources only) | `/var/lib/picolet/manifests/<app>/` |
```

- [ ] **Step 2: Update the bundle layout block.**

Around `:174-184`, update the directory listing:
```text
services/<name>/
  containers/
  volumes/
  networks/
  kube/
  systemd/
  secrets/
  manifests/    # K8s YAML only — validated against k8s.io/api types
  files/        # opaque, container-mounted files; validated only as YAML if .yml/.yaml
  picolet.yml
```

And the line just below ("`manifests/` may contain nested directories. The other six category directories must contain files directly."):
```
`manifests/` and `files/` may contain nested directories. The other six category directories must contain files directly.
```

- [ ] **Step 3: Update the template helper table.**

Around `:298-308`, add a row for `filePath`:
```
| `filePath(relPath)` | Return the absolute deployed path for a file (handles rootless/rootful automatically). `relPath` is relative to the service's `files/` dir |
```

- [ ] **Step 4: Update the hot-reload hook example.**

Around `:247-253`, replace the VictoriaMetrics example to use the new `files:` trigger:
```yaml
  - name: victoriametrics-scrape-reload
    files: [config/scrape.yml]
    unit: victoriametrics.service
    action: http
    method: GET
    url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/-/reload'
    health_url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/health'
```

- [ ] **Step 5: Update the hooks-section prose.**

Around `:263-267`, update the trigger list and the manifest paragraph:
```
Each hook must specify at least one trigger — `secrets`, `manifests`, or `files`.
When more than one is set, the hook fires if ANY listed secret OR manifest OR file changed.

The `manifests` field uses paths relative to the service bundle's `manifests/`
directory (e.g., `app/deployment.yml`); use `manifests/` only for Kubernetes
resources fed to `podman kube play`. The `files` field uses paths relative to
the service bundle's `files/` directory (e.g., `config/scrape.yml`); use `files/`
for arbitrary container-mounted config (Prometheus scrape configs, vmalert rules,
etc.).
```

Around the `Supported actions` table (the `Required fields` column at `:271-275`), update each row that says "at least one trigger (`secrets` and/or `manifests`)" to read "at least one trigger (`secrets`, `manifests`, and/or `files`)".

- [ ] **Step 6: Verify the README still renders.**

```
git diff README.md | head -100
```

Spot-check that nothing got mangled.

- [ ] **Step 7: Commit.**

```
git add README.md && git commit -m "docs(readme): document files/ bundle category"
```

---

### Task 18: Final verification

**Files:** none modified.

- [ ] **Step 1: Run the full suite from a clean cache.**

```
task test
```

Expected: every package green.

- [ ] **Step 2: Run lint.**

```
task lint
```

Expected: no findings.

- [ ] **Step 3: Confirm `grep` cleanliness.**

```
grep -rn 'ManifestRelPath\|ChangedManifests\|changedManifests\|manifestRelPath\|manifestDestPath\|manifestRef' --include='*.go' .
```

Expected: zero hits.

- [ ] **Step 4: Confirm the validator still rejects non-K8s YAML in `manifests/`.**

Run the existing manifest validator tests explicitly:

```
TAGS="remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" go test -tags "$TAGS" ./pkg/validator/ -run TestValidateManifest -v
```

Expected: green. (These cover the K8s strict-mode behavior that must not regress.)

- [ ] **Step 5: Ensure the working tree is clean.**

```
git status
```

Expected: `nothing to commit, working tree clean`.

- [ ] **Step 6: Summary commit (optional).**

If you've been making fixup commits, optionally squash via interactive workflow. Otherwise, leave the commit history as-is — each task = one commit is the cleaner record.

---

## Reporting Deliverables (for the iuk-gitops follow-up)

Once this plan is merged and a picolet release is cut, hand the following to the iuk-gitops migration:

- Category name: `file`
- Bundle subdir: `files/`
- Hook trigger key: `files:`
- Template helper: `filePath(relPath)`
- Deployed path: `/var/lib/picolet/files/<rel>` (rootful) / `~/.local/share/picolet/files/<rel>` (rootless)
- `assignments.yml` top-level key: `files:` (parallel to `manifests:`)
