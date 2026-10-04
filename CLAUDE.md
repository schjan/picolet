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

**Regenerate mocks** (after changing interfaces in `pkg/applier`, `pkg/agent` or `pkg/machine`): `go tool mockery`

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
9. **Apply** — writes ordered by the category table's `ApplyRank` (network → volume → image → build → secret → systemd → manifest → file → pod → container → kube), then `DaemonReload`, then triggered builds (see Category Table; a failed build fails the apply → rollback), then restart changed units (alphabetical; start order comes from the Quadlet-generated dependencies; members of a restarting pod are left to the pod's `Wants=`), then rebuilt images' consumers
10. **State save** — atomic JSON write (tmp + rename) with new SHA + managed file hashes

### Category Table

`pkg/config/categories.go` holds the one category table (destination, validator `Check`, `ApplyRank`, `ConvertOrder`, `PreConvert`, health class, restart policy, resource-name prefill, unit naming) plus the extension map; every consumer derives from it. Rows with `CheckUnsupported` (`.artifact`) are known to Podman but rejected by the validator; the row is already filled in from Podman's `ConvertArtifact` (a oneshot pull service that sets its resource name, like `.image`: `PreConvert`, `HealthReportOnly`, `RestartChanged`), so enabling it means setting `CheckQuadlet` and wiring its converter in `quadletConverters` (`pkg/validator/quadlet.go`), after which `paths:` entries and Service Bundles select it by extension (`config.CategoryForPath`: a first segment `manifests/`/`files/`/`secrets/` wins, otherwise the extension map; a bundle is expanded like a `paths:` directory, bundle-relative); `Subdir` exists only on the manifest/file/secret rows (the first segment that selects them). `assignments.yml` groups take only `paths:`, `secrets:` and `services:` (`pkg/config/assignments.go`), so no category needs a schema field. `ConvertOrder` must equal `quadlet.SupportedExtensions` — a test fails on a Podman upgrade that adds or reorders extensions. Containers and pods are both `PreConvert`: the pre-pass converts containers first, filling each pod's `ContainersToStart`, so the pod's recorded dependencies list its members. `.image` is `PreConvert` (its resource name comes from conversion) and `.build` is `Prefill` (its `ImageTag`), so `Image=x.image`/`Image=x.build` resolve. `.build`/`.image` are `HealthReportOnly`: the health loop reports a unit that failed on its own under the external-activation skip reason and never restarts it, but retries a failed apply-time restart (a `PendingUnits` record) when the row's `Restart` is `RestartChanged` (`.image`) — failed restarts recorded in `PendingUnits` always need a health-loop retry path (`health.restartableByHealth`). `.build` is `RestartRebuild` (`pkg/applier/build.go`): when the unit or a file it builds from (`File=`, build context dir) changes, apply *starts* the build service (a restart would propagate through `Requires=` and stop the consumers before the build succeeds); a failed build fails the apply and puts every tag the apply's builds write back on its previous image (`PodmanClient.ImageID`/`ImageTag`/`ImageUntag`). Then running consumers (units that `Requires=` the build, from `applier.WithDependencies`) are restarted with job mode `ignore-dependencies`, so the build is not run again; consumers not running, members of a restarting pod and timer-run one-shots take the ordinary path or are skipped. A `.build` never lands in `PendingUnits`, and the health loop never restarts it.

Validation takes a `validator.Target{Rootless, HostDataDir}`; callers fill `HostDataDir` from `resolver.ResolvedHost.HostDataDir`, never recompute it. With it, `pkg/validator/build.go` fails a `.build` whose Containerfile (every path `podman build` would try for `File=`; without `File=`, the context's `Containerfile`/`Dockerfile` when the context holds delivered files), named build context (`buildunit.Paths.Context` when `NamedContext`) or `[Service] WorkingDirectory=` lies in a delivered data directory (`<HostDataDir>/files|manifests`) but is not delivered; a path outside those dirs, or one it cannot resolve from the unit (URLs, stdin `-`, specifiers, the service's default directory), may exist on the Host and passes. An empty `HostDataDir` (validator unit tests, tests on hand-filtered file sets) skips that check.

`pkg/buildunit` is the one model of which local paths a `.build`'s `podman build` reads (context directory, Containerfile candidates), in Podman's two stages with their two URL notions (Quadlet's `quadlet.URL` for `ConvertBuild`, `podman build`'s prefix check plus stdin `-` for what it reads from elsewhere); the validator's delivery check and the applier's rebuild trigger both use it, never their own path rules (the validator itself only reads `[Service] WorkingDirectory=`, which systemd must chdir into). A `.build` names data files by host path while `Change.DestPath` is where the agent writes: callers pass the resolved `DataDir`/`HostDataDir` to `applyWithRollback`, which sets `applier.WithHostDataDir`, so a containerized agent's rebuild trigger compares like with like.

### Orphan Detection & Ownership Markers

Quadlet files are written to `/etc/containers/systemd/picolet/` (picolet-owned subdir). Systemd files get `# Managed by picolet` prepended (`config.PicoletMarker`). Secrets are labeled `managed-by=picolet`. At startup, `pkg/orphan` scans for and removes Orphans (Managed Files/secrets no longer in the Fleet); it removes files only and stops no services (stopping orphaned Quadlet services is #185). A symlinked owned directory is left untouched and reported as an error, which ends that scan run. The validator rejects the agent's own container (`config.DefaultSelfUnits`) declaring `Pod=`, since restarting or stopping the pod would stop the agent.

### Interface Ownership

The three system-boundary interfaces of the Agent are **defined and implemented in `pkg/applier`**:

- `SystemdManager` — D-Bus systemd control (`DBusSystemdManager`)
- `PodmanClient` — Podman socket API (`SocketPodmanClient`)
- `FileWriter` — atomic file writes (`AtomicFileWriter`)

Other packages (`agent`, `health`, `rollback`) consume these interfaces. Mocks are generated by `mockery` in `mocks/applier/`.

Note: `SocketPodmanClient` stores a `connCtx context.Context` because the Podman binding library embeds the socket connection into the context — this is intentional (`//nolint:containedctx`).

`bootstrap machine` has its own boundary: `machine.HostOps` (`pkg/machine`, implemented by `OSHostOps`, mock in `mocks/machine/`) — the Machine's users, sessions, units and paths, a read side (checks) and a write side (applies, root). `machine.New` is the planner (Fleet + Machine + options → ordered steps with stable ids, data only, writing nothing; it reads `--secrets-dir` through an `fs.FS` that must confine symlinks — an `os.Root` in production — into a `StepDir` per subdirectory and a `StepCredential` per file, `Step.Content` never printed; a `--secrets-dir` inside the checkout is refused, since setup makes the checkout world-readable); `machine.Evaluate` derives each step's check from its kind and annotates it would do / already done / unknown (`ErrUnprivileged`); `machine.Run` checks each step the same way and applies it through the write side when not done — a failing step stops its Host only, a credential file's check picks its fix (`credentialFix`: write the content, which marks its Host "Agent restart required", or set owner/mode in place via `SetOwnerMode`), and the per-Host bootstrap phase runs `picolet bootstrap` in a container of the Host's own Podman (`RunAsUser` via `runuser`, `RunAsRoot` for the rootful Host) with the Agent quadlet's bind mounts (`agentDirs`' `mount`), so the `state.json` it seeds is keyed by container-internal paths, never `$HOME/...`; then it restarts an Agent marked "restart required" and waits for its health at the address `bootstrap.ResolveAgent` reads from the Fleet-rendered Agent config, and the summary lists every Host (user, port, health, restart). Writes into a Host user's home go through `os.Root` (`EnsureDir`/`WriteFile`/`SetOwnerMode` take a base and a path below it; `EnsureDir` and `SetOwnerMode` open the final element `O_NOFOLLOW`, `WriteFile` renames over it and refuses a directory there), so a symlink the user planted never leads root out of the home. `OSHostOps`' commands (`useradd`, `loginctl`, `runuser`, `systemctl`, `podman run`) are not exercised by CI (no root); verify them on a scratch VM.

### Function Types as Interfaces

`SecretReader` and `DiskReader` in `pkg/resolver` and `pkg/rollback` are function types, not interfaces:

```go
type SecretReader func(path string) (string, error)
```

### Configuration Layers

- **Agent config** (`/etc/picolet/config.yml`) — hostname, repo URL, poll interval. Loaded by `pkg/agentcfg`.
- **Fleet config** (in git repo) — `fleet.yml` (images, ports), `assignments.yml` (file mappings per role/feature), `hosts/<name>/host.yml` (per-host settings incl. `machine:`/`user:`/`listen_port:`). Loaded by `pkg/config`. Production code opens the repo with `config.OpenRepo` (an `os.Root`-backed FS), never `os.DirFS`: `DirFS` follows symlinks out of the tree, which would let a Fleet deploy any host file (the agent runs as root). Host secrets go through `resolver.DirSecretReader`. Tests may use `fstest.MapFS`/`os.DirFS`. Loading is strict; only the Agent's reconciliation passes `config.LenientHosts()` (via `ResolveParams.LenientHosts`), so an unknown `host.yml` key only warns there (older Agents must keep loading a Fleet that adopts a new key) and fails everywhere else. Every Host needs an Agent listen port: `listen_port:` or `fleet.yml` `ports.picolet_metrics` (user Hosts) / `ports.picolet_system_metrics` (rootful), so test fleets carry `picolet_system_metrics`.
- **Secrets** — read from local filesystem (`cfg.SecretsDir`), not from git.
- **Bootstrap credentials** — `host.yml` `bootstrap:` / `fleet.yml` `bootstrap.files|roles` (`config.BootstrapFiles`, effective block in `HostConfig.Bootstrap`, most specific level wins whole; nil = no block = the Host gets the Machine's provider token, `{}` = declared, gets nothing). Read only by `pkg/machine`; providers are injected as `machine.Providers` (production wiring in `pkg/cli`), so tests never open 1Password or pass-cli.

### Template System

Files ending in `.tmpl` are rendered with Go `text/template` (`missingkey=error`). All templates share a single `template.Template` registry enabling cross-references.

Custom functions: `readFile`, `renderTemplate`, `indent`, `readSecretFile`, `siblings` (other Hosts on this Host's Machine, sorted by hostname), `has` (slices.Contains for feature checks).

Template data root: `.Host` (hostname, role, features, machine, user, rootful, listen_port, services, systemd_units), `.Fleet` (full config + all hosts), `.Images`, `.Ports`. `.Host.SystemdUnits` is populated by a first render pass — see `prepareTemplateData` in `pkg/resolver/resolver.go`.

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

`pkg/state`, `pkg/gitpoll`, `pkg/status` and `pkg/buildunit` are fully standalone. `pkg/status` is a process-local in-memory runtime store consumed by `pkg/agent`, `pkg/dashboard`, and `pkg/metrics`.

`pkg/metrics` exposes Prometheus collectors. Custom collectors that read runtime state accept a `*status.Store` at construction (`metrics.Register(store)`); no package-level mutable state. `pkg/metrics` imports `pkg/status` (one direction; no cycle).

Everything else flows through `pkg/agent` which orchestrates the pipeline.
