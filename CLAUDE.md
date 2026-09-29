# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Picolet is a single-binary GitOps agent for managing Podman Quadlet files on Raspberry Pi fleets. Think Flux/ArgoCD, but for hosts running Podman instead of Kubernetes.

Module path: `github.com/schjan/picolet`

## Build & Test

Requires [Task](https://taskfile.dev/) runner. All commands use shared build tags defined in `Taskfile.yml`.

```bash
task build          # native binary
task build-arm64    # cross-compile for RPi (linux/arm64)
task test           # all tests with -race -count=1
task lint           # golangci-lint (v2 config in .golangci.yml)
task lint:fix       # golangci-lint with --fix
task fmt            # run all formatters (gofumpt + gci) via golangci-lint fmt
```

**Regenerate mocks** (after changing interfaces in `pkg/applier`): `go tool mockery`

**Without Task**: all `go build`/`go test` commands require the build tags from `Taskfile.yml` (podman/v5 otherwise pulls in C libraries):

```bash
TAGS="remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper"
go test -tags "$TAGS" ./pkg/reconciler -run TestName -race -count=1   # single test
go test -tags "$TAGS" ./... -update                                    # update golden files (goldie)
```

## Code Style & Linting

- **Formatters**: `gofumpt` + `gci` (import grouping: stdlib / external / localmodule) — run `task fmt` to apply automatically, never reorder imports by hand
- **Aggressive linters**: `cyclop`, `funlen`, `nestif`, `gosec`, `ireturn`, `dupl`, `containedctx`, `contextcheck` — use `//nolint:lintername` with an explanation comment when suppression is justified
- All tests must use `t.Parallel()` (enforced by `tparallel` linter)

## Architecture

### Reconciliation Pipeline

The agent runs a timer-based loop (`pkg/agent`). Each tick:

1. **Health enforce** — restart inactive managed units (5min cooldown per unit)
2. **Git poll** — fetch + hard reset to remote HEAD, compare SHA
3. **Failure gate** — skip if same SHA failed 3+ times
4. **Config load** — `fleet.yml`, `assignments.yml`, `hosts/<name>/host.yml` from repo FS
5. **Resolve** — merge assignments (base → role → features), render templates
6. **Diff** — compare desired files against `state.ManagedFiles` by SHA-256 content hash
7. **Validate** — quadlet files via `quadlet.Convert*()`, K8s manifests via strict unmarshal, systemd units structurally
8. **Snapshot** — save current disk state for rollback
9. **Apply** — writes ordered by the category table's `ApplyRank` (network → volume → image → build → secret → systemd → manifest → file → pod → container → kube), then `DaemonReload` + restart changed units (alphabetical; start order comes from the Quadlet-generated dependencies; members of a restarting pod are left to the pod's `Wants=`)
10. **State save** — atomic JSON write (tmp + rename) with new SHA + managed file hashes

### Category Table

`pkg/config/categories.go` holds the one category table (destination, validator `Check`, `ApplyRank`, `ConvertOrder`, `PreConvert`, health class, restart policy, resource-name prefill, unit naming) plus the extension map; every consumer derives from it. Rows with `CheckUnsupported` (`.artifact`) are known to Podman but rejected by the validator; enabling one means setting `CheckQuadlet` and wiring its converter in `quadletConverters` (`pkg/validator/quadlet.go`), after which `paths:` entries select it by extension (`config.CategoryForPath`: a first segment `manifests/`/`files/`/`secrets/` wins, otherwise the extension map); a `Subdir` plus a typed list in `AssignmentGroup` (`pkg/config/assignments.go`) is only needed for the typed lists and bundle subdirectories until #147/#148. `ConvertOrder` must equal `quadlet.SupportedExtensions` — a test fails on a Podman upgrade that adds or reorders extensions. Containers and pods are both `PreConvert`: the pre-pass converts containers first, filling each pod's `ContainersToStart`, so the pod's recorded dependencies list its members. `.image` is `PreConvert` (its resource name comes from conversion) and `.build` is `Prefill` (its `ImageTag`), so `Image=x.image`/`Image=x.build` resolve. `.build`/`.image` are `HealthReportOnly`: the health loop reports a unit that failed on its own under the external-activation skip reason and never restarts it, but retries a failed apply-time restart (a `PendingUnits` record) when the row's `Restart` is not `RestartNone` — failed restarts recorded in `PendingUnits` always need a health-loop retry path (`health.restartableByHealth`). `.build` is `RestartNone` at apply (rebuild-on-change is #127).

### Orphan Detection & Ownership Markers

Quadlet files are written to `/etc/containers/systemd/picolet/` (picolet-owned subdir). Systemd files get `# Managed by picolet` prepended (`config.PicoletMarker`). Secrets are labeled `managed-by=picolet`. At startup, `pkg/orphan` scans for and removes Orphans (Managed Files/secrets no longer in the Fleet); it removes files only and stops no services (stopping orphaned Quadlet services is #185). A symlinked owned directory is left untouched and reported as an error, which ends that scan run. The validator rejects the agent's own container (`config.DefaultSelfUnits`) declaring `Pod=`, since restarting or stopping the pod would stop the agent.

### Interface Ownership

All three system-boundary interfaces are **defined and implemented in `pkg/applier`**:

- `SystemdManager` — D-Bus systemd control (`DBusSystemdManager`)
- `PodmanClient` — Podman socket API (`SocketPodmanClient`)
- `FileWriter` — atomic file writes (`AtomicFileWriter`)

Other packages (`agent`, `health`, `rollback`) consume these interfaces. Mocks are generated by `mockery` in `mocks/applier/`.

Note: `SocketPodmanClient` stores a `connCtx context.Context` because the Podman binding library embeds the socket connection into the context — this is intentional (`//nolint:containedctx`).

### Function Types as Interfaces

`SecretReader` and `DiskReader` in `pkg/resolver` and `pkg/rollback` are function types, not interfaces:

```go
type SecretReader func(path string) (string, error)
```

### Configuration Layers

- **Agent config** (`/etc/picolet/config.yml`) — hostname, repo URL, poll interval. Loaded by `pkg/agentcfg`.
- **Fleet config** (in git repo) — `fleet.yml` (images, ports), `assignments.yml` (file mappings per role/feature), `hosts/<name>/host.yml` (per-host settings). Loaded by `pkg/config`. Production code reads the repo through `os.OpenRoot(dir)` + `root.FS()`, never `os.DirFS`: `DirFS` follows symlinks out of the tree, which would let a Fleet deploy any host file (the agent runs as root). Tests may use `fstest.MapFS`/`os.DirFS`.
- **Secrets** — read from local filesystem (`cfg.SecretsDir`), not from git.

### Template System

Files ending in `.tmpl` are rendered with Go `text/template` (`missingkey=error`). All templates share a single `template.Template` registry enabling cross-references.

Custom functions: `readFile`, `renderTemplate`, `indent`, `readSecretFile`, `has` (slices.Contains for feature checks).

Template data root: `.Host` (hostname, role, features, services, systemd_units), `.Fleet` (full config + all hosts), `.Images`, `.Ports`. `.Host.SystemdUnits` is populated by a first render pass — see `prepareTemplateData` in `pkg/resolver/resolver.go`.

### Error Patterns

- Wrap with `fmt.Errorf("context: %w", err)` consistently
- One custom error type: `*resolver.HostNotFoundError` (check with `errors.As`)
- `errors.Join` for accumulating validation errors in `pkg/validator`
- Result structs with `Errors []error` fields for non-fatal errors (health checks, apply)

### Testing Patterns

- **Unit tests**: mockery-generated mocks with `.EXPECT()` fluent API; `pkg/applier` has an in-package `memFileWriter` for self-testing
- **Integration tests**: `integration_test.go` at root uses `testdata/example-fleet/` with `goldie` golden-file snapshots
- **Agent integration**: `pkg/agent/agent_test.go` creates a real in-memory git repo via `go-git`, exercises the full loop

### Package Dependencies

`pkg/state`, `pkg/gitpoll`, and `pkg/status` are fully standalone. `pkg/status` is a process-local in-memory runtime store consumed by `pkg/agent`, `pkg/dashboard`, and `pkg/metrics`.

`pkg/metrics` exposes Prometheus collectors. Custom collectors that read runtime state accept a `*status.Store` at construction (`metrics.Register(store)`); no package-level mutable state. `pkg/metrics` imports `pkg/status` (one direction; no cycle).

Everything else flows through `pkg/agent` which orchestrates the pipeline.
