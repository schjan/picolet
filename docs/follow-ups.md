# Picolet Follow-Ups

Internal tracker for refactors and improvements deferred from earlier
work. Each entry names a file:line, the issue, and a sketch of the
intended fix. Prune entries as they land.

## From PR #71 review (reload-config branch)

### F1. Rename overlapping restart-set maps in `runHooksWithPending`
- **Where:** `pkg/applier/applier.go` — `runHooksWithPending`
- **What:** The hot loop juggles three maps (`restartScheduled`, `restartSet`, `restartUnits`) with overlapping semantics; readability suffers.
- **Suggested fix:** rename to `priorRestarts` / `cumulativeRestarts` / `hookRestarts`, or introduce a `restartTracker` value type that owns all three and exposes intent-named methods.

### F2. Replace `firstSetField` + `hookField` with a simpler helper
- **Where:** `pkg/config/hook.go:201-208`
- **What:** Three callers, one error message; named `hookField` struct exists only to hold two strings.
- **Suggested fix:** `checkForbiddenFields(hookName string, pairs ...string)` taking interleaved `(fieldname, value)` pairs.

### F3. `Hook` flat 12-field struct → action-variant sum types
- **Where:** `pkg/config/hook.go:26-39`
- **What:** Action-specific fields (`URL`/`Method`/`HealthURL` for http; `Container`/`Signal` for signal) are flat on `Hook`. Every consumer (`hookExecutionKey`, `Normalize`, validator) re-encodes per-action validity.
- **Suggested fix:** sum-type pattern via embedded action structs and a sealed interface. Significant refactor — touches YAML deserialization (custom `UnmarshalYAML`) and every consumer.

### F4. Test fixture helpers in `pkg/applier/applier_test.go`
- **Where:** `pkg/applier/applier_test.go`
- **What:** `config.Hook{...}` literal is repeated near-verbatim in 8+ tests; what differs (`OnFailure`, `HealthURL`, `Action`) is buried in copy-paste.
- **Suggested fix:** small fixture constructors (`testHTTPHook(name, secret string) config.Hook`, `testSignalHook(...)`).

### F5. `RunPendingHooks(ctx, hooks, names)` — hooks as parameter
- **Where:** `pkg/agent/agent.go retryPendingHooks` constructs `applier.New` per call
- **What:** Only `hooks` varies between calls; the throwaway constructor obscures the real dependency.
- **Suggested fix:** `Applier.RunPendingHooks` takes `hooks` directly; the agent reuses one `Applier` value across ticks.

### F6. Document or remove `enforceRetryBudget` mutation contract
- **Where:** `pkg/agent/agent.go:822-848`
- **What:** `mergePendingHooks` returns a fresh map; `enforceRetryBudget` mutates its input. Both are correct today but the asymmetric API is a footgun for future callers.
- **Suggested fix:** make `enforceRetryBudget` return a fresh map too, or document the in-place contract on the function.

### F7. Per-call `context.WithTimeout` instead of shared client timeout
- **Where:** `pkg/applier/hook.go:14-17, 98-118`
- **What:** `httpClient.Timeout = 5s` covers both the reload POST and the health-check GET. A 4-second reload + 3-second health is a spurious failure.
- **Suggested fix:** drop `Timeout` from `http.Client`, use per-call `context.WithTimeout` so each request gets its own budget.

### F8. Bounded poll-until-healthy instead of fixed 2s sleep
- **Where:** `pkg/applier/hook.go:103-112`
- **What:** Hard `time.NewTimer(healthDelay)` is wasteful when the service is already healthy and insufficient when it takes longer.
- **Suggested fix:** poll the health URL on a 200ms cadence up to a per-hook budget; succeed on first 2xx.

### F9. Validate `Hook.Unit` suffix and stricter container cross-check
- **Where:** `pkg/config/hook.go:68-70`, `pkg/resolver/resolver.go validateSignalHookContainer`
- **What:** A typo like `unit: foo.servce` reaches D-Bus before failing. Signal-hook container validation is lenient when `ContainerName=` is unset (Quadlet's default-naming rule is not re-implemented).
- **Suggested fix:** allowlist of known systemd suffixes (`.service`, `.timer`, `.target`, …) and Quadlet extensions (`.container`, `.kube`, …) in `Normalize`. Re-implement Quadlet's default `ContainerName=` derivation in the resolver helper to validate the unset case.

### F10. Rename `restartFallbackUnits`; add `picolet.service` self-restart guard
- **Where:** `pkg/applier/applier.go:442-457`
- **What:** Function fires on successful `Action: restart` hooks too, not only fallback. Lacks the `picolet.service` self-restart guard the main `restartUnits` carefully wraps.
- **Suggested fix:** rename to `restartHookTargets`; share the self-restart goroutine pattern from `restartUnits` if a future hook ever targets `picolet.service`.

### F11. `validateHookHTTPURL` allowlist / metadata-IP block
- **Where:** `pkg/config/hook.go:159-171`
- **What:** Hook URLs are validated only for scheme/host non-empty. `http://169.254.169.254/...` (cloud metadata) and `http://10.0.0.0/8/...` are accepted. Defense-in-depth, not a CVE: hook URLs come from the same trust boundary as the fleet repo.
- **Suggested fix:** documented allowlist (e.g., loopback + per-fleet declared hosts) or blocklist (link-local, multicast, metadata IPs).

### F12. E2E expansion: keep_running retry, on_failure: restart fallback, manifest trigger
- **Where:** `e2e/hooks_test.go`
- **What:** Existing e2e tests cover only happy-path restart and HTTP. The retry-budget exhaustion path, fallback-restart on hook failure, and manifest-triggered hooks have no e2e coverage.
- **Suggested fix:** add three e2e scenarios using `httptest.Server` returning 500 (for retry exhaustion), a hook with `on_failure: restart` (for fallback path), and a manifest-triggered hook with a real systemd unit on the test bench.

## From issue #159 (timer one-shot run metrics)

### F13. Last-success history does not survive an agent restart
- **Where:** `pkg/status/status.go` — `Store.ObserveRun` / `Snapshot.Runs`
- **What:** `SucceededAt` is derived across observations and lives only in the in-memory status store, as #159 specifies ("these values live in their own map in the status store"). systemd itself keeps no success history: `Result=` holds only the current run's state. So for a job that succeeded, then failed, then had picolet restarted, the `picolet_unit_last_success_timestamp_seconds` series disappears until the job succeeds again — which weakens #142's "metrics survive an Agent restart" story in exactly the failing-backup case the alert is for. The same gap appears when a job's next run is already in flight at the first observation: no success can be credited to a run picolet never saw finish.
- **Suggested fix:** persist the per-unit `SucceededAt` (and last completed `Result`) in `state.json` alongside `LastPrunedAt`, and seed the store from it on startup the way `seededPrunedAt` does. Alternative: reconstruct from journald, which is heavier and not worth it.