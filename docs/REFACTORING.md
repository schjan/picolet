# Refactoring Roadmap

Outcome of a full codebase review (July 2026). The review ran three deep-dives
(core pipeline, integrations, test strategy), was adversarially cross-checked
against the code, and produced two batches of work:

- **Done on `claude/codebase-review-refactor`**: dead-surface deletions
  (`PodmanClient` interface shrink, `gitpoll.NewWithToken`, the
  `resolver.OpSecretReader` alias), `pkg/metrics` collector-file merge, small
  idiom cleanups, resolver API unexports, the `tick()` breakup, the
  `pending.go` extraction, and the `rollback.ApplyWithSnapshot` consolidation.
- **This document**: the larger items, in priority order, for future PRs.

## Overall verdict

The codebase is in good shape. The core primitives (`reconciler`, `rollback`,
`orphan`, `health`, `state`, `validator`, `gitpoll`, `config`,
`internal/atomicfile`) are small and single-purpose; atomic writes are shared
through `internal/atomicfile`; error wrapping, `slog` usage, and context
discipline are consistent. The test pyramid is well-shaped: a wide unit base,
an agent state-machine layer against an in-memory git repo with mocked
system boundaries, and a thin e2e top against **real rootless Podman + real
`systemd --user` on arm64 CI** — the same architecture as the Pi fleet.

The items below are ordered by expected payoff.

## 1. e2e: replace the self-clone with a local git fixture (HIGH)

`e2e/pipeline_test.go` clones `github.com/schjan/picolet` at the branch under
test. Consequences: the branch must be pushed before e2e means anything,
GitHub or docker.io hiccups flake the suite, and `testdata/example-fleet`
changes are not exercised until pushed.

Fix: seed a local bare repo (`git.PlainInit`) from `testdata/example-fleet`,
the way `pkg/agent/agent_test.go` (`initTestRepo`) already does, and point the
poller at it. Keep one real-remote clone test for the auth path.

## 2. e2e: real rollback scenario (HIGH)

Rollback is the one product-critical path covered only with mocks
(`TestAgentRollbackOnApplyFailure`, plus the new `rollback.ApplyWithSnapshot`
unit tests). Add an e2e scenario: deploy image A → introduce a change whose
apply fails fatally → assert the container is back on image A and state still
points at the previous SHA.

## 3. e2e: split `TestE2EPipeline`; drive `agent.Run()` once (MEDIUM)

`TestE2EPipeline` is ~800 lines of order-dependent subtests sharing mutable
host state — an early failure blinds everything downstream. Split it into
independent scenarios using the isolated-dir pattern from
`e2e/hooks_test.go` (`uniqueTestQuadletDir` + `agent.WithDirs`). While there:

- Exercise the full timer-driven `agent.Run()` loop once in e2e (today only
  `poller.Poll` + `ReconcileOnce` are driven separately; loop scheduling is
  verified only against mocks).
- Pre-pull or pin images (or run a local registry) to remove docker.io
  rate-limit flakes.

## 4. Shared test-fixture builder (MEDIUM)

~75 hand-written `fstest.MapFS` fleet fixtures live across
`pkg/resolver/resolver_test.go`, `integration_test.go`, `pkg/config` tests and
friends. A `host.yml`/`assignments.yml` schema change touches dozens of them.

Introduce `internal/fleettest`: a small builder (hosts, assignments, service
bundles, files) that can emit a `fstest.MapFS`, a real directory, or a git
repo. Migrate opportunistically — new tests first, old ones when touched.
`testdata/example-fleet` stays the shared golden fleet.

## 5. Test coverage for `pkg/cli/runners.go` (MEDIUM)

~650 LOC with a single small unit test. `runApply`/`runDown` get real coverage
via `TestE2EApplyDown`, but these are untested: `appendMQTTOptions`,
`appendGitHubOptions`, `opReaderFromConfig`/`ppReaderFromConfig`, `runTrigger`,
`runHealthcheck`, `runDryRun`, and the dashboard wiring. The option-assembly
helpers are pure given a config value and are cheap to table-test.

## 6. Flatten `agentcfg` GitHub-App validation (LOW-MEDIUM)

The rule is simple — configure the GitHub App via exactly one of direct
fields / 1Password refs / Proton Pass refs, mutually exclusive with any
git_token source — but it is spread across ~12 small helpers in
`pkg/agentcfg/agentcfg.go` (`validateGitHubApp` … `countSet`). Rewrite as one
table-driven validator. Also dedupe the (app_id, installation_id,
private_key) ref-triple, which is spelled out in ~4 places including
`pkg/githubauth/config.go`.

This is correctness-critical config validation: change it with tests first.

## 7. Provider table in the resolver (LOW-MEDIUM)

The 1Password/ProtonPass branch pair repeats in ~4 places in
`pkg/resolver/resolver.go` (`secretDestPath`, `splitDirectRefs`,
`buildSecretFiles`, `collectTemplateRefs`) while `registry.go` carries a
generic `ProviderTemplate` slice — so adding a third provider today means
touching all the hardcoded sites anyway. Either iterate a small provider
table (IsRef, parse → `PodmanSecretName`) at those sites, or drop the generic
layer. Do this when adding a provider, not before.

## 8. Centralize category properties (LOW)

The 8-category enum is re-listed in ~6 places: `reconciler.Categories`,
`applier.categoryOrder`, two overlapping tables in `pkg/resolver`
(`quadletCategoryPaths`, `buildStandardFiles`), `services.go`, and switches in
`validator`/`applier`. A typed properties table in `pkg/config` (apply-order
rank, isQuadlet, bundle subdir, usesRelPath) would make adding a ninth
category compiler-checked instead of a 6-site hunt.

## 9. Naming clarifications (LOW, opportunistic)

- `pkg/reconciler` is a differ; the orchestration lives in `pkg/agent`. If
  `pkg/agent` keeps growing, rename `reconciler` → `changeset` and extract the
  orchestration state machine into a real `reconciler` package.
- `resolver.Config.OpSecretReader`/`PPSecretReader` field names predate
  multi-provider support; a neutral scheme would read better.

Not worth standalone churn — fold into other work.

## 10. CI polish (LOW)

Add `govulncheck`; consider coverage reporting. The arm64 runner + real
podman/systemd e2e job is the right architecture — keep it.

## Non-goals (reviewed, confirmed good — do not "fix")

- **onepassword vs protonpass**: no shared client code wanted. One wraps the
  official SDK; the other shells out to `pass-cli` and owns session probing
  and secret redaction. The `resolver.SecretRefReader` closure is already the
  minimal common abstraction — do not introduce a `SecretProvider` interface.
- **github / githubauth split**: correct layering (client vs config adapter).
- **mqtt, bootstrap, agentcfg detect/hostdatadir**: deliberate, well-guarded
  complexity (reconnect/LWT ordering, self-unit protection, container
  bind-mount detection). Leave as-is.
- **status → dashboard/metrics fan-out**: one in-memory store, two read-only
  projections. Correct design.
- **Mock usage**: the five mocked interfaces are all real system boundaries
  (D-Bus, Podman socket, file writes, MQTT broker, GitHub deployments).
- **Dependencies**: `mochi-mqtt` and `goldie` are test-only direct deps by
  design; `sigs.k8s.io/yaml` (strict k8s manifests) and `go.yaml.in/yaml`
  (generic YAML) serve different purposes.
- **`onepassword.Client` finalizer guard**: protects against a real SDK
  footgun; keep, including its GC test.
