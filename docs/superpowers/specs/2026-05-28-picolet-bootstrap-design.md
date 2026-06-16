# `picolet bootstrap`: one-shot host provisioning

**Status:** Draft
**Date:** 2026-05-28
**Author:** Jannis Schäfer
**Scope:** picolet feature (new subcommand family) + small targeted auto-detection in `pkg/agentcfg`. Backward-compatible with all existing fleet repos.

## Background

To bring a new host into a picolet-managed fleet today, an operator runs `deploy/bootstrap/bootstrap.sh` (or its rootless counterpart). That script:

1. Requires the operator to hand-write `/etc/picolet/config.yml` (separate from the `picolet_config` Podman secret the fleet repo's template will eventually deploy).
2. Writes the picolet quadlet to `/etc/containers/systemd/picolet.container` — a path **outside** picolet's owned subdir (`/etc/containers/systemd/picolet/`), so it falls outside both the managed-files map and the orphan scanner's purview.
3. Starts the unit and exits, with a comment in the script that says "First reconcile will self-restart once — this is expected."

That last note is the symptom of a real problem: once picolet starts and runs its first reconcile, the resolver renders the fleet repo's `picolet.container.tmpl`, decides to write it to `/etc/containers/systemd/picolet/picolet.container` (the picolet-owned subdir), creates the `picolet_config` Podman secret, daemon-reloads, and restarts itself. The bootstrap-deployed quadlet at the top-level path persists indefinitely, configured to use a file-mounted config that drifts from the secret-delivered one.

The net effect: the bootstrap script leaves behind durable drift, requires the operator to construct configuration that overlaps with the fleet repo's `picolet_config.yml.tmpl`, and produces an avoidable self-restart on first reconcile. This spec replaces it with a `picolet bootstrap` command family that fixes all three.

## Goals

- Single-command (per host, per mode) bootstrap that exits when picolet is verified healthy.
- Reuses the fleet repo as the source of truth — no parallel config schema, no hand-templated YAML.
- No self-restart on first reconcile: state.json is seeded such that picolet's own files appear already-managed.
- Symmetric `teardown` for clean removal (and for e2e tests).
- Workstation-side `bootstrap create` that emits a ready-to-paste invocation tailored to the target node, with optional thin SSH delivery.
- Two minimal picolet-startup auto-detections (`systemd_user` and `host_data_dir`) that let fleet-repo `picolet_config` templates drop user-specific hardcoding (`/home/<user>/...`) without forcing any migration. `Rootless` deliberately stays explicit because it drives resolver path-layout, which must not be flipped under the feet of a containerized rootless deployment.
- Backward-compatible: existing fleet repos using `deploy/bootstrap/*.sh` continue to work; new flow is opt-in.

## Non-goals

- No replacement for Ansible / Pyinfra / Salt. `bootstrap create` emits a script; the optional `--ssh` flag is a thin convenience wrapper, not a fleet-orchestration platform.
- No new YAML schema for bootstrap-time configuration. Auth, repo URL, MQTT config etc. live in the fleet repo's `picolet_config.yml.tmpl` and nowhere else.
- No clone or git-fetch inside the `bootstrap` subcommand itself. Bring Your Own Clone: the operator supplies a local checkout. (Picolet itself still clones/fetches at runtime — that uses the auth the fleet repo's template configures.)
- No template-consolidation work in the fleet repo (merging `picolet_config.yml.tmpl` and `picolet_system_config.yml.tmpl`; moving `repo.url` into `fleet.yml`). Deferred to a separate spec; this work creates the cleaner baseline that effort will operate on.
- No state-schema changes. State seeding writes the same `state.json` shape the agent already produces.
- No new clone-time auth code. Bootstrap doesn't need auth at all for cloning (operator brings a local clone). The deliberate design rule is that bootstrap never resolves 1Password or Proton Pass refs at template time either — those pass through to the deployed Podman secret as YAML values and are resolved by picolet at runtime. See "Provider secret handling" for the full rule and the explicit error bootstrap surfaces if a template breaks it.

## User-facing design

### The Bring Your Own Clone model

Bootstrap's only true auth dependency is the initial clone of the fleet repo. Eliminating that dependency eliminates bootstrap's entire auth surface. The operator clones the fleet repo themselves (workstation or target — wherever they have repo access) using their normal git workflow, then points `picolet bootstrap` at the local directory.

```
operator workstation                            target host
─────────────────────                           ───────────
git clone <fleet repo>          ───rsync───►    /tmp/fleet/
picolet bootstrap create                        (operator places host-managed
  --hostname rpi5-1-system                       secrets at /etc/picolet/secrets/)
  → prints script with                          podman run --rm <volumes> \
    target invocation                             picolet bootstrap \
    (explicit --service)                          --hostname=rpi5-1-system \
                                                  --repo-dir=/tmp/fleet \
                                                  --service=picolet-system
                                                ↓
                                                picolet bootstrap:
                                                  1. resolve picolet service for host
                                                  2. apply via existing pipeline (no restart)
                                                  3. seed state.json (leave AppliedSHA empty)
                                                  4. enable + start the unit
                                                  5. wait for /health
                                                  6. exit
                                                ↓
                                                picolet service running;
                                                first agent reconcile applies
                                                every other service.
```

After bootstrap exits, picolet runs as a normal systemd service. Its first reconcile sees the picolet service's files already in `state.ManagedFiles` (no diff for itself, no restart) and applies every other service the host's assignments demand.

### Three subcommands

```
picolet bootstrap create --hostname <name> [flags...]  # workstation: emit a copy-paste script
picolet bootstrap --hostname <name> --repo-dir <path>  # target: actually bootstrap
picolet bootstrap teardown --hostname <name> ...       # target: undo
```

The `--hostname` flag is consistent across all three; it always refers to the value of `hosts/<name>/host.yml`'s `hostname` field (which is also what `agentcfg.Config.Hostname` carries). If `--node` reads more naturally for `bootstrap create`, it can be wired as a CLI alias — that's a cosmetic decision and doesn't change anything else in this spec.

### `picolet bootstrap create`

Workstation-side helper. Pure-Go and read-only; performs no network access **unless `--ssh` is given** (in which case it shells out to `rsync` and `ssh` to deliver and run the script — see the `--ssh` description). Reads the local fleet repo, runs the new service-scoped `resolver.ResolveServicesForHost(hostname, [picoletService])` against it, and emits a tailored deployment plan. Crucially, `create` does NOT call the full-host `ResolveHost` — otherwise an unrelated service in the host's assignments with a broken template or an inline `readOpSecret` could block generating a picolet bootstrap script the operator needs. Service scope keeps `create`'s blast radius identical to `bootstrap`'s.

**Secret-resolution mode differs from target `bootstrap`.** `create` runs on the operator's workstation, where the target's host-managed secret files (`/etc/picolet/secrets/...`) do not exist. So create renders with a **provider-strict, file-tolerant** resolver:
- Provider helpers (`readOpSecret`, `readProtonPassSecret`) still error — an inline provider call indicates a template that breaks BYOC's auth-free property, and that's worth catching at create time too.
- `readSecretFile` returns a placeholder instead of erroring when the file is absent. Create only inspects the config's *structure* (which fields are set → metrics port + secret checklist), never the resolved values, so a placeholder is sufficient. The real strict render with actual files happens on the target during `bootstrap`.

```
picolet bootstrap create --hostname <name>
    [--fleet-dir <path>]              # default: cwd
    [--service <name>]                # override the picolet/picolet-system default
    [--target-path <path>]            # default: /tmp/fleet
    [--script]                        # output bare runnable script (no header comments)
    [--ssh <user@host>]               # optional: rsync + ssh-exec the script
    [--skip-git-checks]               # skip the pre-flight clone-state checks (dirty / ahead / behind)
```

Default output is annotated for human reading; `--script` strips comments and emits a `set -euo pipefail` shell script suitable for piping (`ssh user@target bash -s`).

**Determinations made:**

- **Rootful vs rootless** — by the resolved service name (`picolet-system` → rootful, `picolet` → rootless). `host.yml`'s `pi_type` is informational; it doesn't drive the determination.
- **Service to deploy** — defaults to whichever of `picolet` / `picolet-system` appears in the host's resolved assignment set (a given `host.yml` has exactly one `pi_type`, so exactly one is present). Overridable via `--service`. Must exist in the resolved host's bundles; otherwise create errors with the available service names. **The generated invocation always carries an explicit `--service=<name>`**, so the target-side default never has to guess.
- **Image** — read from `fleet.yml`'s `images.picolet` entry.
- **Host-side paths for the `-v` mounts** — picolet's rootful and rootless quadlets follow stable conventions today (rootful uses `/etc/...`, `/var/lib/picolet[-system]`, system sockets; rootless uses `~/.config/...`, `~/.local/share/picolet`, `$XDG_RUNTIME_DIR/...`). `bootstrap create` emits these conventional paths directly. If a fleet diverges from convention (e.g. moves the systemd dir), the operator edits the generated script before running it. Parsing the rendered quadlet's `Volume=` directives to auto-derive divergent paths is a future enhancement; for now, convention covers every fleet we have.
- **Host-managed secret checklist** — after resolving the fleet repo, parse the rendered `picolet_config` content as `agentcfg.Config` (structural, not regex). Walk the populated fields and emit one checklist line per host-managed credential, labelled by auth mode:
  - `cfg.GitTokenPath` → "Personal Access Token for git" + path
  - `cfg.OnePassword.TokenPath` → "1Password service account token" + path
  - `cfg.ProtonPass.PATPath` → "Proton Pass PAT" + path
  - `cfg.GitHubPrivateKeyPath` → "GitHub App private key (PEM)" + path
  Provider refs in the config (`op://`, `pass://`) are reported as informational lines ("git token will be resolved at runtime via 1Password"); no host file is required for them beyond the provider token already listed.
- **Pre-flight clone-state check** — when `--fleet-dir` is a git checkout, verify the working tree exactly matches the upstream remote. Three checks, all must pass:
  1. `git status --porcelain` is empty (no uncommitted changes or untracked tracked-paths).
  2. `git rev-list --count @{upstream}..HEAD` is `0` (no unpushed commits).
  3. `git rev-list --count HEAD..@{upstream}` is `0` (not behind the remote).
  If any check fails, the local clone diverges from what picolet will fetch on first reconcile. The "ahead" and "dirty" cases would cause picolet's first reconcile to rewrite its own files with different rendered output and trigger the self-restart this spec is designed to prevent; the "behind" case has the same effect for a different reason — picolet fetches newer bytes than the operator deployed. Behaviour: fail by default with a clear explanation of which check failed; `--skip-git-checks` bypasses **all three** (dirty, ahead, and behind), useful for development/CI. (The flag is deliberately named for what it does — skip the git safety checks — rather than `--allow-dirty`, which would misleadingly imply it covers only uncommitted changes.) Non-git checkouts (e.g. operator transferred a tarball) skip the checks entirely with a one-line warning.

**Output example (rootful target):**

```
# Bootstrap plan for rpi5-1-system (pi_type=node-system, rootful)
# Fleet repo: /Users/jannis/src/jannis/gitops (HEAD: a32ca09)
# Picolet image: ghcr.io/schjan/picolet:0.1.27
# Service: picolet-system

# Step 1 — Transfer the fleet repo to the target:
rsync -a --delete ./ rpi5-1-system:/tmp/fleet/

# Step 2 — Place host-managed secrets on the target (parsed from picolet_system_config):
#   /etc/picolet/secrets/git_token   (required: git_token_path)

# Step 3 — On the target, run as root:
sudo podman run --rm \
  -v /tmp/fleet:/repo:ro \
  -v /etc/picolet:/etc/picolet \
  -v /var/lib/picolet-system:/var/lib/picolet \
  -v /etc/containers/systemd:/etc/containers/systemd \
  -v /etc/systemd/system:/etc/systemd/system \
  -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket \
  -v /run/podman/podman.sock:/run/podman/podman.sock \
  --network host \
  ghcr.io/schjan/picolet:0.1.27 bootstrap \
    --hostname=rpi5-1-system \
    --repo-dir=/repo \
    --service=picolet-system

# Watch:
sudo journalctl -fu picolet-system
```

For rootless, the rsync target lands in the user's home, mounts use `$HOME/.config/...` and `$XDG_RUNTIME_DIR/podman/...`, no `sudo` is emitted, and the generated command carries `--service=picolet` (no `--rootless` — the container sees `/etc/...` internally; see "systemd_user auto-detection").

**`--ssh` mode (thin):** rsync + ssh-exec, using the operator's `~/.ssh/config` for everything (port, key, jump host, agent forwarding). No additional SSH flags on the picolet side. Rootful target with non-root SSH user: `sudo` is prepended; non-passwordless sudo will produce a clear error message. Operators with anything fancier fall back to `--script` and pipe it through their own tooling.

### `picolet bootstrap`

Runs on the target inside `podman run --rm`. Two required flags; everything else has sensible defaults or is auto-detected.

```
picolet bootstrap
    --hostname <name>                       # required; must match a host in the fleet repo
    --repo-dir <path>                       # required; local fleet-repo directory inside container
    [--service <name>]                      # default: derived from resolved systemd mode (user → picolet, system → picolet-system)
    [--systemd auto|user|system]            # default: auto (D-Bus presence check); see "systemd_user auto-detection"
    [--rootless]                            # default: false (correct for the containerized bootstrap case)
    [--podman-socket <path>]                # default: /run/podman/podman.sock
    [--secrets-dir <path>]                  # default: /etc/picolet/secrets (uniform inside the bootstrap container)
    [--health-path <path>]                  # default: /health
    [--metrics-port <port>]                 # default: read from rendered picolet_config; fall back to agent default
    [--timeout <duration>]                  # default: 90s
    [--data-dir <path>]                     # default: rootless-aware (when --rootless is set) / /var/lib/picolet otherwise
    [--allow-restart]                       # restart the picolet unit if it is already active and the diff is non-empty (see "Re-runs with a running picolet")
```

`--systemd` is a tri-state override for which systemd to manage. `auto` (default) runs `detectSystemdUser()` (D-Bus presence). `user` forces the user systemd; `system` forces the system D-Bus. Use the explicit forms only for edge cases — auto-detection is correct for every standard rootful/rootless Podman invocation.

`--rootless` is the path-layout knob. It mirrors the YAML `rootless` field and stays a plain bool because bootstrap is almost always run inside a container (BYOC + `podman run`), where `rootless=false` is correct (container internal paths are `/etc/...`, `/var/lib/...`). Operators running bootstrap natively as a regular user pass `--rootless` to flip the resolver's path-layout switch.

Note the `--service` default keys off the resolved **systemd mode**, not `--rootless`: user systemd → `picolet`, system systemd → `picolet-system`. (Keying off `--rootless` would be wrong, because `--rootless` defaults to `false` even for a containerized rootless deployment — see "systemd_user auto-detection".) When the operator uses `bootstrap create`, the generated invocation always carries an explicit `--service`, so this default only matters for hand-run `bootstrap` invocations.

`--podman-socket` is the in-container path to the Podman API socket. Default `/run/podman/podman.sock`, matching the convention all the picolet container quadlets use (the rootless quadlet maps `%t/podman/podman.sock` to that path; the rootful one bind-mounts `/run/podman/podman.sock` directly). Override only if the operator has chosen a non-standard mount.

The `--metrics-port` flag is an override. By default, bootstrap parses the rendered `picolet_config` (or `picolet_system_config`) as `agentcfg.Config` and uses its `metrics_port` value. This is essential because rootful picolet-system uses port 9418, rootless picolet uses 9417, and hardcoding either default would make the health probe fail for one of the two modes. The flag exists for unusual cases where the operator needs to override.

The `--secrets-dir` flag points to the *in-container* path where the host's picolet secrets directory is mounted. By convention bootstrap is invoked with `-v <host-secrets-dir>:/etc/picolet/secrets`, matching what the picolet container itself does. The flag lets the resolver's `readSecretFile` helper read host-managed secret files at template-render time — useful for templates that legitimately inline non-provider values (e.g. an MQTT password from a host file). If the directory isn't mounted or the requested file is absent, `readSecretFile` fails with a clear error.

**Internal flow:**

1. **Load fleet config.** `config.LoadAll(os.DirFS(repoDir))` — same as `picolet validate`. No clone, no fetch.
2. **Resolve systemd mode, rootless, and service.** `--systemd auto` → call `detectSystemdUser()`; `user`/`system` → use directly. `rootless` comes from `--rootless` (default false; only true when explicitly passed, since containerized bootstrap should always use the in-container `/etc/...` paths). `--service`, if unset, defaults from the resolved systemd mode (user → `picolet`, system → `picolet-system`); `bootstrap create` always passes it explicitly.
3. **Resolve host, scoped to the picolet service only.** Bootstrap MUST NOT do a full host resolve here — see the dedicated "Service-scoped resolve" subsection below for why and how. Result: a `[]resolver.ResolvedFile` containing only the picolet service's files. Provider helpers error (`Strict: true`); `readSecretFile` reads from `--secrets-dir` (see "Provider secret handling" for the two-knob split).
4. **Derive metrics port.** Find the rendered `picolet_config` secret file in the resolved set; parse its content as `agentcfg.Config` (via a new `agentcfg.Parse(bytes)` export of the existing parser). Take `cfg.MetricsPort` if non-zero; otherwise fall back to `agentcfg`'s default. The `--metrics-port` CLI flag, when set, takes precedence over both.
5. **Validate filtered files.** `validator.ValidateFiles(files, rootless)` — same validation the agent applies.
6. **Connect to systemd + podman.** `applier.NewDBusSystemdManager(ctx, useSystemdUser)` and `applier.NewSocketPodmanClient(ctx, podmanSocket)`. Same constructors as the agent.
7. **Acquire the bootstrap lock.** Same lock file (`<data-dir>/reconciliation.lock`) the agent and `picolet apply` use, so bootstrap cannot race with a daemon that's already running on the host.
8. **Load existing state, diff.** `state.NewStore(statePath).Load()` returns the existing state (or an empty one if absent). Diff `Diff(filteredFiles, existingState)` — on a fresh host the diff is all creates; on a re-run it's a no-op or a delta only for what genuinely changed.
9. **Running-unit guard.** If the diff is non-empty AND the picolet/picolet-system service is already active (`systemd.UnitState` — small new helper), require explicit `--allow-restart`. Without the flag, fail with a message that explains why: applying changes to picolet's quadlet or `picolet_config` while the service runs would leave the in-memory process out of sync with what state.json records — and nothing inside picolet would later notice (health-enforce only checks active state, not content drift). The operator's options are listed in the error: (a) stop the unit first and re-run bootstrap, (b) pass `--allow-restart` to let bootstrap restart picolet after applying, or (c) accept that this re-run is a no-op by skipping the apply.
10. **Write resources without restarting.** Call a new `applier.ApplyWithoutRestarts(ctx, changeset)` mode that writes files, creates/updates Podman secrets, and runs `daemon-reload`, but **does not** start or restart any systemd unit. This is the critical sequencing fix: starting `picolet.service` before state.json exists would have picolet load with empty state and immediately try to re-create its own files, racing the bootstrap. (See "Apply pipeline modifications" below for the implementation.) When invoked with `--allow-restart` against an active picolet, call this same method — bootstrap explicitly restarts the unit in step 14.
11. **Seed state.json.** `state.MergeChangeset(existing, changeset)` overlays the changeset onto the existing state — entries in the changeset are added/updated, entries the changeset didn't touch are preserved. This is a NEW helper (`agent.UpdateState` rebuilds the map from the changeset and would drop every non-picolet entry after the agent had reconciled the rest of the host). **Do not touch `state.AppliedSHA`** — leaving the existing value (empty on a fresh host) guarantees picolet's first poll triggers a full diff against the (then-empty-for-everything-else) state and applies the rest of the host's services. Save via `store.Save`.
12. **Optionally seed `/var/lib/picolet/repo`** with a copy of `--repo-dir` (including `.git/` if present). Pure optimisation — picolet's first poll does `fetch+reset` instead of a fresh clone. Skip when the source isn't a git checkout.
13. **Release the bootstrap lock.** State is durable on disk; picolet must be able to acquire the lock when it starts.
14. **Enable and start (or restart) the unit.** Always call `systemd.EnableUnit(ctx, serviceName)` (new method on `applier.SystemdManager`, see "Apply pipeline modifications") so the service comes up on reboot — this matches the current `bootstrap.sh` behaviour (`systemctl enable --now picolet.service`). Quadlet-generated unit files carry an `[Install]` section, so enable-via-D-Bus creates the appropriate `target.wants` symlink against the path the systemd-generator produced. Then, if the unit was inactive at step 9, `systemd.StartUnit`. If it was active and `--allow-restart` was passed, `systemd.RestartUnit`. If it was active and `--allow-restart` was not passed, the diff was empty (step 9 enforced this), so no action is needed.
15. **Probe `/health`.** Poll `http://localhost:<metrics_port><health_path>` until 200 or `--timeout` expires. Same endpoint `picolet healthcheck` and the Quadlet `HealthCmd=` use. On timeout: fail with the last-observed response (status code + body excerpt) for diagnosability.
16. **Exit zero.** The picolet service is now enabled, running, healthy, and aware of its own files. The agent loop owns everything from here on.

**Idempotency.** Re-running `picolet bootstrap` against an already-bootstrapped host: step 8's diff against existing state.json produces zero changes (resolver output matches state.json hashes), step 9's guard passes trivially (no diff → no need for `--allow-restart`), step 10's apply is a no-op, step 11 saves an unchanged state, step 14's enable is a no-op (already enabled), the health probe succeeds quickly, exit zero. Safe to retry.

### Re-runs with a running picolet

The interesting case is: bootstrap was run successfully a while ago, picolet has been running, the operator updates the fleet repo's `picolet.container.tmpl` (e.g. bumps the picolet image version), and re-runs `picolet bootstrap` on the target. Three outcomes are possible, and bootstrap is explicit about which one is happening:

1. **Diff is empty** — fleet repo hasn't actually changed picolet's files. No-op, exit zero.
2. **Diff is non-empty, `--allow-restart` not passed** — fail at step 9. Operator gets a clear message: bootstrap will not silently update files for an active picolet, because state.json would then claim "current" while the running process is still on the old bytes, and nothing inside picolet would surface the drift. (The agent's health-enforce tick only checks systemd active state, never content hashes.)
3. **Diff is non-empty, `--allow-restart` passed** — apply the changeset (suppressing restart per step 10), seed state, then explicitly restart the unit in step 14. After restart, picolet loads with the seeded state and the new bytes; first reconcile diffs cleanly.

This makes bootstrap's contract loud: it never produces a stale-running-with-current-state situation.

### Service-scoped resolve

Bootstrap must resolve only the picolet service's files, not the whole host's assignment set. Two reasons:

1. **Strict secret-resolution coverage.** Bootstrap's strict resolver (errors on `readOpSecret` / `readProtonPassSecret` with nil readers) is the right rule for the picolet service itself. But a full host resolve renders every service's templates — and if any OTHER service in the host's assignments uses `readOpSecret`, bootstrap would fail even though bootstrap doesn't need that service. The operator's n8n container shouldn't break their picolet bootstrap.
2. **Wasted work + wasted error surface.** Bootstrap doesn't apply anything except picolet; rendering the entire fleet is gratuitous and risks confusing errors from services bootstrap will never touch.

Implementation: a new resolver entry point that takes an explicit service allow-list, then renders only those bundles' files. Sketch:

```go
// pkg/resolver/resolver.go
//
// ResolveServicesForHost resolves only the listed services for the given host.
// The host's pi_type and features are still loaded (so template data exposes
// .Host.PiType, .Host.Features as usual), but only the listed services'
// bundles are walked and rendered.
//
// Strict secret-resolution mode (from rc.Strict) applies to the rendered subset.
func (r *Resolver) ResolveServicesForHost(ctx context.Context, hostname string, services []string) (ResolvedHost, error)
```

The existing `ResolveHost` continues to mean "everything assigned" and is used by the agent and `picolet apply`. Bootstrap calls the new entry point with `[]string{"picolet"}` or `[]string{"picolet-system"}`. The implementation shares the inner rendering logic with `ResolveHost`; the only difference is the service-list provided to the bundle walker.

If the requested service is not part of the host's resolved assignment set, return a clear error listing the assigned services — bootstrap surfaces this to the operator.

### `picolet bootstrap teardown`

```
picolet bootstrap teardown
    --hostname <name>
    [--service <name>]
    [--systemd auto|user|system]            # default: auto
    [--rootless]                            # default: false (for the containerized bootstrap case)
    [--podman-socket <path>]                # default: /run/podman/podman.sock
    [--data-dir <path>]
```

1. Acquire the config lock (same as `picolet down`).
2. Call `systemd.DisableUnit` so the picolet/picolet-system unit doesn't come back on reboot (symmetric to bootstrap's enable; idempotent on already-disabled units). Disabling before stopping (vs after) avoids a brief window where systemd could restart the unit between stop and disable on units with `Restart=always`.
3. Run `reconciler.Diff(nil, state)` — every managed file becomes a "delete" action.
4. Apply the deletes via `applier.ApplyWithoutRestarts`. The pre-delete `StopUnit` phase is preserved by `ApplyWithoutRestarts` (see "Apply pipeline modifications"), so each unit is properly stopped *before* its quadlet file is removed — this is essential for full-uninstall correctness when teardown runs on a host where picolet had reconciled additional services. After the stops, the deletes proceed: quadlet files removed from disk, `picolet_config` Podman secret deleted, `daemon-reload`. Post-apply restart phase is skipped (everything is being torn down, no restarts to fire).
5. Remove `state.json` and `/var/lib/picolet/repo` (best-effort).
6. Release the lock.
7. Do NOT touch `/etc/picolet/secrets/` — those are operator-owned.

For a host where picolet has been running and reconciled additional services, `bootstrap teardown` removes everything picolet tracks — this is the appropriate full-uninstall behaviour. For the e2e test, the test's fleet repo only defines the picolet service, so state never grows past it.

## Implementation outline

### New package: `pkg/bootstrap`

```
pkg/bootstrap/
    bootstrap.go        // Run, Teardown — the on-target operations
    create.go           // Create — the workstation-side helper
    filter.go           // Service filtering by SrcPath
    health.go           // /health polling loop
    bootstrap_test.go
    create_test.go      // golden-file tests for `bootstrap create` output
```

Each function takes a `Config` struct constructed from CLI flags, so the CLI layer (`pkg/cli/bootstrap.go`) stays thin and the package is independently testable.

### CLI wiring: `pkg/cli/`

Add `bootstrapCmd()`, `bootstrapCreateCmd()`, `bootstrapTeardownCmd()` returning `*cli.Command`. Wire them into the existing root command in `newApp()`. `bootstrap create` uses text logging; `bootstrap` and `bootstrap teardown` use JSON logging (daemon-style, mirrors `runCmd`/`applyCmd`).

### Service filtering

A `ResolvedFile.SrcPath` looks like `services/picolet/containers/picolet.container.tmpl`. Filter helper:

```go
// FilterByService returns the subset of files whose SrcPath belongs
// to the given bundle (resolver bundle name == fleet service directory).
func FilterByService(files []resolver.ResolvedFile, service string) []resolver.ResolvedFile {
    prefix := path.Join("services", service) + "/"
    out := make([]resolver.ResolvedFile, 0, len(files))
    for _, f := range files {
        if strings.HasPrefix(f.SrcPath, prefix) {
            out = append(out, f)
        }
    }
    return out
}
```

If the resolver gains an explicit per-file bundle attribution in the future (already partially present via the bundle loader), the filter switches to that without touching consumers.

### Apply pipeline modifications

Bootstrap can't use `applier.Apply` as-is. That function writes files, creates secrets, daemon-reloads, **and then restarts changed units** — which, for the picolet service itself, means the agent starts up before bootstrap has finished seeding state.json. The agent then loads with empty state, sees its own quadlet as an unmanaged file on disk, and races bootstrap to claim it. Two small additions to `pkg/applier` fix this cleanly:

**1. `ApplyWithoutRestarts`.** A new method (or an option on the existing `Apply`) that suppresses only the **post-apply start/restart** phase. It still performs every other unit-lifecycle step that's necessary for correctness — in particular, **pre-delete `StopUnit` calls are preserved**, so a deletion in the changeset still stops the corresponding unit before the file is removed.

```go
// pkg/applier/apply.go
//
// ApplyWithoutRestarts runs the same pipeline as Apply with one targeted change:
// the post-apply "start/restart changed-or-created units" phase is skipped.
// Everything else — pre-delete StopUnit, file writes, secret create/update,
// daemon-reload, post-delete cleanup — runs exactly as in Apply.
//
// Used by bootstrap to seed the host before explicitly enabling/starting the
// picolet service (so the agent doesn't start before state.json exists), and
// by teardown to remove resources (where post-apply restarts are irrelevant
// because everything is being deleted anyway, but pre-delete stops are still
// required to bring units down cleanly before removing their files).
func (a *Applier) ApplyWithoutRestarts(ctx context.Context, cs *reconciler.Changeset) (*ApplyResult, error)
```

The implementation factors the existing `Apply` into three internal phases — pre-apply stops, write+reload, post-apply starts/restarts — and `ApplyWithoutRestarts` runs phases 1 and 2 only. This matters for the teardown path: a teardown changeset has only Delete actions, every Delete still requires its `StopUnit` (otherwise containers run on orphaned after their unit files vanish, which is exactly the "delete-then-leave-running" hazard the user flagged). The skipped phase only contains post-apply lifecycle calls; deletion stops are part of the deletion itself, not part of a "restart" phase.

No duplication: the daemon-style `Apply` still invokes all three phases in sequence as today.

**2. Three new methods on `applier.SystemdManager`.** Bootstrap's running-unit guard and explicit lifecycle management need helpers the existing interface doesn't expose:

```go
// pkg/applier/systemd.go
type SystemdManager interface {
    // ... existing methods ...

    // EnableUnit creates the persistent target.wants symlink so the unit
    // comes up on boot. Equivalent to `systemctl enable <unit>`.
    EnableUnit(ctx context.Context, unitName string) error

    // DisableUnit removes the persistent enable symlink. Equivalent to
    // `systemctl disable <unit>`. Symmetric helper used by teardown.
    DisableUnit(ctx context.Context, unitName string) error

    // UnitState returns the unit's ActiveState ("active", "inactive",
    // "failed", "activating", "deactivating"). Bootstrap uses this for
    // the running-unit guard at step 9. Returns "inactive" (no error) when
    // the unit is unknown — semantically equivalent for the guard's purpose.
    UnitState(ctx context.Context, unitName string) (string, error)
}
```

`EnableUnit` / `DisableUnit` wrap `EnableUnitFilesContext` and `DisableUnitFilesContext` (with `runtime=false`, persistent symlinks). `UnitState` queries the unit's `ActiveState` D-Bus property via `GetUnitPropertyContext` — picolet already uses this pattern elsewhere for the health-enforce loop, so the underlying access is well-trodden.

Quadlet-generated unit files carry an `[Install] WantedBy=...` section in the source `.container.tmpl`. The systemd generator preserves it in the produced `.service` file, so `EnableUnitFiles` finds the right `WantedBy=` target and creates the symlink under `target.wants/`. This matches what the existing `bootstrap.sh` accomplishes via `systemctl enable --now`.

Bootstrap's usage:
- Step 9 calls `UnitState` to decide whether the running-unit guard fires.
- Step 14 calls `EnableUnit` then either `StartUnit` (inactive → active) or `RestartUnit` (active + `--allow-restart`). `RestartUnit` already exists on the interface.
- Teardown calls `DisableUnit` before deleting the quadlet file.

The mock in `mocks/applier/` gets regenerated to expose the new methods (`go tool mockery` per CLAUDE.md).

### Provider secret handling

Bootstrap deliberately does **not** resolve 1Password or Proton Pass secrets at template-render time. The picolet binary at runtime is the single place where `op://` and `pass://` references get resolved — bootstrap should never need 1Password SDK credentials or a pass-cli session.

Concretely:

- The picolet_config template **may** contain `op://...` / `pass://...` strings as YAML values (e.g. `onepassword.git_token_ref: op://Vault/Item/git-token`). These pass through bootstrap unchanged and are deployed in the Podman secret as-is. Picolet resolves them at runtime.
- The picolet_config template **must not** call `readOpSecret` / `readProtonPassSecret` template helpers inline (e.g. `{{ readOpSecret "op://..." }}`). Inline resolution would force bootstrap to authenticate against 1Password / Proton Pass at bootstrap time, undermining BYOC's auth-free property.
- `readSecretFile` (which reads a host-mounted file) and other non-provider helpers (`readFile`, `renderTemplate`, `indent`, `has`) are NOT governed by the provider-strict flag — they don't reach external systems. Their behaviour depends instead on the file `SecretReader` the caller supplies (next paragraph).

**Implementation: two independent knobs.** Today the resolver accepts nil `OpSecretReader` / `PPSecretReader` and substitutes placeholder values (CI/validate use case).

1. **Provider strictness** — a new `Strict` field on `resolver.Config`. When true, the provider helpers (`readOpSecret`, `readProtonPassSecret`) error if invoked with a nil reader instead of emitting a placeholder. **Both** target `bootstrap` and `bootstrap create` set `Strict: true` with nil provider readers — neither ever resolves provider secrets. The guaranteed clear failure if a template tries inline provider resolution: `"picolet_config.yml.tmpl calls readOpSecret at template time, but bootstrap does not resolve provider secrets. Use a YAML-value reference (e.g. git_token_ref: op://...) instead — picolet will resolve it at runtime."`

2. **File-helper behaviour** — governed by the `SecretReader` (file reader) the caller passes, NOT by `Strict`:
   - **Target `bootstrap`** passes a real file reader rooted at `--secrets-dir`. `readSecretFile` reads the mounted file; a missing file errors (the operator should have placed it per the checklist).
   - **`bootstrap create`** passes a tolerant placeholder reader (workstation has no target secrets). `readSecretFile` returns a placeholder; create only needs config structure, never values.

Existing CI/validate paths continue using `Strict: false` and a nil/placeholder file reader — unchanged behaviour.

### State seeding

`agent.UpdateState` REBUILDS `ManagedFiles` and `ServiceNames` from the changeset — it explicitly clears the existing maps and re-populates them. That's correct for the daemon's full-reconcile pipeline (where the changeset reflects every service the host should have) but wrong for bootstrap (whose changeset reflects only the picolet service). Using `UpdateState` on a re-run after the agent has reconciled the rest of the host would drop every non-picolet entry from state, then the next agent reconcile would re-create everything from scratch, defeating the whole "no churn on re-runs" guarantee.

The fix is a new merge helper that overlays the changeset onto existing state without removing untouched entries:

```go
// pkg/state/merge.go
//
// MergeChangeset overlays applied changes onto an existing state without
// removing entries for paths the changeset didn't touch. Used by bootstrap,
// which only manages a subset of the host's files at a time.
//
// Semantics per change action:
//   - Create / Update: insert or replace the entry at change.DestPath.
//   - Delete: remove the entry at change.DestPath (so teardown works).
//   - Noop: leave the entry untouched.
//
// AppliedSHA is NOT modified by this function — callers decide whether to
// record an SHA via st.MarkApplied. Bootstrap deliberately doesn't.
func MergeChangeset(st *State, changeset *reconciler.Changeset) {
    if st.ManagedFiles == nil {
        st.ManagedFiles = make(map[string]ManagedFile)
    }
    if st.ServiceNames == nil {
        st.ServiceNames = make(map[string]string)
    }
    for _, change := range changeset.Changes {
        switch change.Action {
        case reconciler.ActionDelete:
            delete(st.ManagedFiles, change.DestPath)
            delete(st.ServiceNames, change.DestPath)
        case reconciler.ActionCreate, reconciler.ActionUpdate:
            st.ManagedFiles[change.DestPath] = ManagedFile{Hash: change.NewHash, Category: change.Category}
            // Set OR clear the service-name mapping so an Update that turns a
            // quadlet into a non-quadlet (ServiceName becomes "") doesn't leave
            // a stale entry behind. Symmetric with the ManagedFiles write above.
            if change.ServiceName != "" {
                st.ServiceNames[change.DestPath] = change.ServiceName
            } else {
                delete(st.ServiceNames, change.DestPath)
            }
        }
    }
}
```

Bootstrap's seeding step becomes:

```go
func SeedState(store *state.Store, changeset *reconciler.Changeset) error {
    st, err := store.Load()
    if err != nil {
        return err
    }
    state.MergeChangeset(st, changeset)
    // Note: AppliedSHA deliberately untouched. On a fresh host it's already
    // empty (so the first agent poll triggers a full reconcile); on a re-run
    // after picolet has been live, the agent's last successful SHA stays in
    // place and the next normal poll handles drift correctly.
    return store.Save(st)
}
```

`agent.UpdateState` keeps its current semantics — it's used by the daemon's full-reconcile and `picolet apply` paths where rebuild is the correct behaviour. The new helper is bootstrap-specific.

### Health probe

```go
func WaitForHealth(ctx context.Context, port int, healthPath string, timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    var lastErr error
    for time.Now().Before(deadline) {
        if err := probeOnce(ctx, port, healthPath); err == nil {
            return nil
        } else {
            lastErr = err
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(2 * time.Second):
        }
    }
    return fmt.Errorf("picolet did not report healthy within %s (last error: %w)", timeout, lastErr)
}
```

`probeOnce` is structurally the same code as `runHealthcheck` in `pkg/cli/runners.go`. Worth a small refactor: extract the GET-and-check into a shared helper in `pkg/agent` (or `pkg/health`) so bootstrap's probe and the `healthcheck` subcommand share one implementation.

### Reuse from existing packages

- `pkg/config` for fleet-config loading (`config.LoadAll`).
- `pkg/resolver` — bootstrap calls the new `ResolveServicesForHost` entry point with `Strict: true` and nil provider readers (see "Service-scoped resolve" and "Provider secret handling").
- `pkg/agent.AcquireLock` for the bootstrap-time lock.
- `pkg/validator` for validation.
- `pkg/applier` for the apply pipeline (`ApplyWithoutRestarts` — new variant — plus the existing `NewDBusSystemdManager`, `NewSocketPodmanClient`, `NewAtomicFileWriter`); `SystemdManager.EnableUnit` / `DisableUnit` / `UnitState` are new methods on the existing interface.
- `pkg/reconciler` for diffing against existing state.
- `pkg/state` — bootstrap uses the new `MergeChangeset` helper (overlay semantics) rather than `agent.UpdateState` (rebuild semantics).
- `pkg/agentcfg.Parse` (new export of the existing parser) for reading `metrics_port` out of the rendered `picolet_config`.

Bootstrap writes no validate/diff logic of its own. New code outside `pkg/bootstrap`:
- `pkg/applier`: `ApplyWithoutRestarts`, `SystemdManager.EnableUnit`/`DisableUnit`/`UnitState`.
- `pkg/state`: `MergeChangeset`.
- `pkg/resolver`: `ResolveServicesForHost`, `Strict` field on `resolver.Config`.
- `pkg/agentcfg`: `detectSystemdUser` (auto-detect when `SystemdUser` is nil), `detectHostDataDir`, `Parse` exported, mountinfo helper. `Rootless` stays a plain `bool` with current semantics — auto-detection does not apply to it.

All five are well-scoped additions. `EnableUnit` is a long-standing latent gap — today nothing in picolet actually `systemctl enable`s anything — so the daemon and `picolet apply` paths benefit from it being available too (though wiring them up to use it is out of scope for this spec).

## Picolet startup auto-detection

Two narrowly-scoped runtime additions, both in `pkg/agentcfg`. Both are pure additions: explicit YAML values in `picolet_config` continue to win, so no fleet repo needs to change.

### `systemd_user` auto-detection (not `rootless`)

The first version of this spec auto-detected `Rootless`, but that's wrong. `Rootless` controls more than which systemd to talk to — it also drives **path layout** in the resolver (`pkg/resolver/resolver.go`: `Rootless=true` switches destination paths to `~/.config/...` and `~/.local/share/...` instead of `/etc/...` and `/var/lib/...`). For a containerized rootless picolet (the iuk-gitops / gitops case), the container internally sees `/etc/containers/systemd`, `/etc/systemd/system`, `/var/lib/picolet` — those are bind-mount destinations the rootless quadlet template wires up to `~/.config/containers/systemd` etc. on the host. Auto-setting `Rootless=true` inside that container would make the resolver write to `~/.config/...` paths that don't exist inside the container, and track them in state.json at the wrong locations.

The signal we get from D-Bus presence (system bus mounted vs not) really tells us "**should picolet connect to system or user systemd?**" — which is `SystemdUser`, not `Rootless`. Those two concepts coincide for native deployments but diverge for containerized ones, and picolet supports both.

The corrected design auto-detects `SystemdUser` only. `Rootless` stays explicit and unchanged: still a plain `bool` field, still defaults to `false`, still set in YAML when (and only when) picolet runs natively as a regular user.

```go
// pkg/agentcfg/detect.go
func detectSystemdUser() bool {
    if os.Geteuid() != 0 {
        return true
    }
    // UID 0 — could be: native root, rootful container, or rootless container
    // where in-namespace UID 0 maps to a real user. The signal that decides
    // which systemd to manage is whether the operator wired up the system D-Bus.
    if _, err := os.Stat("/run/dbus/system_bus_socket"); err == nil {
        return false
    }
    return true
}
```

Truth table (what gets detected for `SystemdUser`):

| Scenario | `Geteuid()` | `/run/dbus/system_bus_socket` | `SystemdUser` | Correct |
|---|---|---|---|---|
| Native root, rootful daemon | 0 | yes | false (system) | ✓ |
| Native non-root, rootless | non-0 | (skipped) | true (user) | ✓ |
| Rootful Podman container | 0 (identity uid_map) | yes (operator mounted) | false (system) | ✓ |
| Rootless Podman container | 0 (in user-ns) | no | true (user) | ✓ |
| Rootful container with `--userns=auto` | non-0 (remapped) | unmountable for permission reasons | true | acceptable: this combination can't access host-root resources picolet needs anyway, so it fails at the system-bus connection step regardless |
| Misconfigured: rootful container with no system-bus mount | 0 | no | true | wrong, but the startup connection fails either way — operator gets a different error message |

`SystemdUser` is already a `*bool` in `agentcfg.Config`, so presence tracking is already correct: explicit `true` / explicit `false` / omitted (`nil`, auto-detect). `setDefaults` calls `detectSystemdUser()` when `SystemdUser` is nil.

**`Rootless` does NOT change shape.** It remains `bool`, defaults to `false`. The only consumers of `Rootless=true` are the resolver's path-layout switch and a couple of CLI defaults — both correctly disabled when picolet runs containerized (which is the dominant case). For native-rootless deployments operators continue setting `rootless: true` in YAML.

**Migration story still works.** The iuk-gitops `picolet_config.yml.tmpl` never set `rootless` (it correctly defaulted to false for the containerized case); it set `systemd_user: true` explicitly. With auto-detection, that line goes away — the explicit `systemd_user: true` becomes redundant because the rootless container has no `/run/dbus/system_bus_socket` mount and auto-detection arrives at the same answer. Combined with `host_data_dir` auto-detection, the template drops two lines and the user-specific `drkda` username disappears, exactly as intended. No fleet repo is required to change; existing explicit values still win.

### `host_data_dir` auto-detection

`HostDataDir` is set today so that `filePath`/`manifestPath` template helpers emit host-visible paths when picolet runs containerised. In rootless deployments today this hardcodes the operator's username (`/home/drkda/.local/share/picolet`). It's discoverable directly from the bind-mount's source.

**Implementation must use a proper mountinfo parser**, not raw `strings.Fields`. The kernel escapes whitespace and a few other characters in mountinfo paths (`\040` for space, `\011` for tab, `\012` for newline, `\134` for backslash), and a `strings.Fields` split would either miscount fields or return un-unescaped paths. A small dedicated parser in `pkg/agentcfg` covers this:

```go
// pkg/agentcfg/mountinfo.go (sketch — full parser handles escapes correctly)
type mountInfoEntry struct {
    mountID     int
    root        string  // path within the source filesystem (host path for bind mounts)
    mountPoint  string  // path in this mount namespace
    fstype      string
    source      string  // mount source device or bind source
}

func parseMountinfoLine(line string) (mountInfoEntry, error) {
    // Mountinfo format (per proc(5)):
    //   mountID parentID major:minor root mountPoint mountOpts <optional fields> - fstype source superOpts
    // Fields are space-separated; path-bearing fields (root, mountPoint, source) are escaped.
    // Split on " - " to separate pre-/post-dash sections, then unescape each path field.
    ...
}

func detectHostDataDir(dataDir string) string {
    f, err := os.Open("/proc/self/mountinfo")
    if err != nil {
        return ""
    }
    defer f.Close()
    sc := bufio.NewScanner(f)
    for sc.Scan() {
        e, err := parseMountinfoLine(sc.Text())
        if err != nil {
            continue
        }
        if e.mountPoint == dataDir {
            return e.root  // host path of the bind mount
        }
    }
    return ""
}
```

**Effective `data_dir` for the lookup.** `detectHostDataDir` must be called with picolet's *effective* `data_dir` — the value after defaults have applied, so the mountinfo lookup matches whatever the operator actually bind-mounted to. The function ordering inside `Config.setDefaults` is: (1) `Rootless` (already either explicit YAML or false default — no auto-detection for `Rootless`), (2) resolve `DataDir` default from `Rootless`, (3) auto-detect `SystemdUser` if nil (D-Bus presence check), (4) auto-detect `HostDataDir` using the resolved `DataDir`. Documented in the function's comment so the dependency isn't accidentally inverted.

`Config.effectiveHostDataDir()` returns, in order: explicit value → detected value → `data_dir` (native case, host path == container path).

### Resulting fleet-repo simplification (opt-in)

After these two changes ship, an iuk-gitops-style `picolet_config.yml.tmpl` can shrink from:

```yaml
hostname: "{{ .Host.Hostname }}"
repo_url: "https://github.com/drk-darmstadt-iuk/iuk-gitops.git"
repo_branch: "main"
systemd_user: true
git_token_path: "/etc/picolet/secrets/git_token"
metrics_port: {{ index .Ports "picolet_metrics" }}
host_data_dir: "/home/drkda/.local/share/picolet"
onepassword:
  token_path: "/etc/picolet/secrets/op_service_account_token"
mqtt:
  broker_url: "tcp://localhost:{{ index .Ports "mosquitto" }}"
```

to:

```yaml
hostname: "{{ .Host.Hostname }}"
repo_url: "https://github.com/drk-darmstadt-iuk/iuk-gitops.git"
git_token_path: "/etc/picolet/secrets/git_token"
metrics_port: {{ index .Ports "picolet_metrics" }}
onepassword:
  token_path: "/etc/picolet/secrets/op_service_account_token"
mqtt:
  broker_url: "tcp://localhost:{{ index .Ports "mosquitto" }}"
```

`repo_branch` was already defaulted. `systemd_user` is now auto-detected via D-Bus presence (the rootless container has no `/run/dbus/system_bus_socket` mounted, so detection returns `true` — same outcome as the explicit `true` it replaces). `host_data_dir` is auto-detected via mountinfo. The user-specific `drkda` is gone, and the template becomes portable across rootless users without edits.

Note that the template never set `rootless` — and still doesn't. `Rootless` continues to default to `false` for the containerized case (which is correct: the container sees `/etc/...` internally; the rootless-ness is in the bind-mount mapping, not in picolet's path-layout decision). Auto-detection only changes `SystemdUser`.

No fleet repo is required to change — this is opt-in cleanup, not a migration.

## E2E test plan

Adds `e2e/bootstrap_test.go`, build-tag `e2e`, same shape as the existing `pipeline_test.go`. Talks to the local Podman socket directly via bindings. It exercises bootstrap at **two levels**:

- **Package level** (`bootstrap.Run` / `bootstrap.Teardown` called as Go functions) — fast, focused assertions on state/secret/file/container outcomes, no image build.
- **CLI level** (`cli.Execute(ctx, []string{"picolet", "bootstrap", "--hostname=...", "--repo-dir=...", ...})` in-process) — exercises the real command entrypoint: flag parsing, defaults, the `bootstrap`/`bootstrap teardown` subcommand wiring in `newApp()`, and the single-command exit behaviour the feature promises. Still no `podman run` and no image build — `cli.Execute` runs the same binary's command tree in-process, exactly as the existing tests invoke the CLI. This closes the gap the package-only test would leave: a broken flag default or unwired subcommand would pass the package test but fail the operator.

Both levels point at the same stub fleet repo and the same local Podman socket.

### Test fixture

`testdata/bootstrap-fleet/` — minimal fleet repo:

```
testdata/bootstrap-fleet/
    fleet.yml              # only the picolet image and metrics port
    assignments.yml        # one pi_type: bootstrap-test → [picolet]
    hosts/
        e2e-bootstrap/
            host.yml       # hostname: e2e-bootstrap, pi_type: bootstrap-test
    services/
        picolet/
            containers/
                picolet.container.tmpl
            secrets/
                picolet_config.yml.tmpl
```

The stub `picolet.container.tmpl` references a public image known to serve HTTP 200 on a configurable port (e.g. `docker.io/library/nginx:alpine`) and a `HealthCmd=true` so Podman's own health gate doesn't gate the test on real picolet behaviour. The stub `picolet_config.yml.tmpl` renders to a minimal valid `agentcfg` (hostname, repo_url, metrics_port — the picolet binary is not actually run; the stub container is). The bootstrap probe uses `--health-path=/` to hit nginx's default page.

### Sub-tests (sequential)

1. **prep** — creates a temp fleet-repo dir from `testdata/bootstrap-fleet/`, pre-cleans any leftover container/secret from a prior interrupted run (`picolet-e2e-bootstrap`, `picolet_config_e2e`).
2. **bootstrap (package)** — calls `bootstrap.Run` with the temp repo and a test-only `--data-dir`. Asserts:
   - state.json exists at the expected path, has the picolet service's files in `ManagedFiles`, has empty `AppliedSHA`.
   - The stub container is running (Podman API).
   - The `/` probe returned 200 within timeout.
   - The quadlet file exists at the expected path on disk.
   - The unit is enabled (`UnitState` / enablement check).
3. **idempotent (package)** — calls `bootstrap.Run` again with the same inputs. Asserts: zero changes applied, exit zero, state.json unchanged.
4. **teardown (package)** — calls `bootstrap.Teardown`. Asserts: quadlet file removed, Podman secret removed, state.json removed, stub container no longer running, unit disabled.
5. **cli_roundtrip** — runs the full cycle through the real command entrypoint: `cli.Execute(ctx, []string{"picolet", "bootstrap", "--hostname=e2e-bootstrap", "--repo-dir=<temp>", "--data-dir=<temp>", "--health-path=/", ...})` then `cli.Execute(ctx, []string{"picolet", "bootstrap", "teardown", ...})`. Asserts the same post-conditions as steps 2 and 4, plus that both commands return nil error (the single-command success contract). This verifies flag parsing, defaults, and subcommand wiring — not just the package functions.
6. **teardown_idempotent** — calls teardown again on an already-cleaned host (both `bootstrap.Teardown` and the CLI form). Asserts no error, no panic.

Cleanup hook removes any residual container/secret/file even on test failure, using the same pattern as `pipeline_test.go`'s `t.Cleanup`.

`bootstrap create` is covered by a separate (non-e2e, no build tag) golden-file test in `pkg/bootstrap/create_test.go` — it's pure formatting against a known fleet repo, no Podman needed. A thin CLI-level create test (`cli.Execute` with `bootstrap create`, asserting the generated script contains the expected `podman run` invocation and secret checklist) covers the command wiring for create.

## Out of scope (and explicit followups)

These deserve their own brainstorm + spec; they're real but not on the critical path for landing bootstrap.

1. **Fleet-level repo metadata.** Move `repo.url`/`repo.branch` into `fleet.yml`; templates reference `{{ .Fleet.Repo.URL }}`. Eliminates the remaining duplication between `picolet_config` and `picolet_system_config`. Requires a small resolver change and a template-data extension. Useful but not blocking.
2. **Template consolidation.** Merging the rootful and rootless `picolet_config` templates into a single shared one. Two templates differ on `metrics_port` and (until this spec) `systemd_user`/`host_data_dir`. After this spec the divergence is just the port — a cross-service shared-template mechanism (or a small bit of `{{ if }}` logic) could fuse them. Touches the resolver's template registry; not trivial.
3. **`--ssh` enhancements.** Per-key, jump host, port — only if real demand materialises. For now operators with non-default SSH needs use `--script` and their own tooling.
4. **Removing `bootstrap.sh` / `bootstrap-rootless.sh`.** Once `picolet bootstrap` is documented and adopted, `deploy/bootstrap/*.sh` can be retired. Deferred to avoid breaking existing users mid-flight.
5. **GitHub Actions integration.** A reusable workflow that runs `bootstrap create --ssh ...` from CI on push to fleet repos. Future ergonomics, not in this spec.

## Backward compatibility

- All existing fleet repos continue to work unchanged. Auto-detection applies only to `systemd_user` and `host_data_dir`, and only when the YAML field is unset; an explicit `systemd_user: true`/`false` or `host_data_dir: /home/...` continues to win. `rootless` is NOT auto-detected — it keeps its current explicit `bool` semantics (default `false`), so `rootless: true` deployments are entirely unaffected.
- The existing `deploy/bootstrap/*.sh` shell scripts remain in-tree until at least one release cycle after `picolet bootstrap` documentation lands. They are not deleted as part of this spec.
- No state-schema bump, no metric rename, no breaking CLI change. `picolet bootstrap` is purely additive.
- The fleet-repo simplification shown above (dropping `systemd_user`, `host_data_dir`, `repo_branch`) is optional. Adopting it requires updating the `picolet_config.yml.tmpl` in the fleet repo and reconciling once.

## Open questions

None at the time of writing. The brainstorm and four spec-review passes resolved:
- BYOC vs in-bootstrap clone (BYOC chosen).
- State seeding vs sentinel-file gating (state seeding via new `state.MergeChangeset` overlay helper, not `agent.UpdateState` rebuild; `AppliedSHA` left at its existing value).
- Auth surface for cloning (eliminated via BYOC).
- Provider secret resolution at bootstrap time (eliminated via strict-resolver + pass-through rule; bootstrap errors on inline provider helpers). Two independent knobs: provider strictness (`Strict` flag, on for both create and target) vs file-helper behaviour (governed by the supplied `SecretReader` — real on target via `--secrets-dir`, tolerant placeholder on create).
- Service scope of resolve, for both `bootstrap` and `bootstrap create` (`ResolveServicesForHost` — strict mode applies only to the picolet bundle; unrelated services with broken templates or inline provider helpers can't block bootstrap or create).
- Secret-resolution asymmetry between create and target bootstrap: create is provider-strict but file-tolerant (`readSecretFile` returns placeholder — target secrets aren't on the workstation; create only needs config structure); target `bootstrap` does the real strict render with `--secrets-dir`.
- E2E coverage at both package and CLI-entrypoint levels (`cli.Execute` round-trip), not package functions alone.
- `--ssh` scope (single-flag, thin wrapper; everything else delegated to the operator's tooling).
- What auto-detection actually controls: `SystemdUser`, NOT `Rootless`. `Rootless` drives path layout and stays explicit (default `false`, correct for containerized bootstrap); `SystemdUser` auto-detects from D-Bus presence and lets templates drop `systemd_user: true` lines.
- Mountinfo parsing (proper unescaping parser, not `strings.Fields`; correct ordering inside `setDefaults`).
- Apply-pipeline sequencing for picolet's own service (new `ApplyWithoutRestarts` mode + explicit enable/start after state.json is durable + `UnitState`-based guard for already-running picolet with explicit `--allow-restart` override).
- `ApplyWithoutRestarts` semantics: suppresses post-apply starts/restarts only; pre-delete stops are preserved so teardown brings units down cleanly before removing their files.
- Bootstrap CLI completeness: `--podman-socket`, `--secrets-dir`, `--systemd`, `--rootless` all explicit; defaults documented.
- Metrics-port derivation (read from rendered picolet_config via `agentcfg.Parse`; override via CLI flag).
- Idempotency (diff against existing state.json, not against empty state).
- Pre-flight clone-state check in `bootstrap create` (fail on dirty / unpushed / behind; `--skip-git-checks` bypasses all three; bidirectional upstream comparison).
- Template helper names (`readOpSecret`, `readProtonPassSecret`, `readSecretFile`) and state field name (`AppliedSHA`) match the code.
- Template consolidation extent (limited to two auto-detections; deeper consolidation deferred).
