# CLAUDE.md

## Project

Picolet is a single-binary GitOps agent for managing Podman Quadlet files on Raspberry Pi fleets.

Module path: `github.com/schjan/picolet`

## Build

Picolet requires CGO_ENABLED=0 and specific build tags because it depends on `containers/podman/v5` but only uses the remote (socket) client. Without these tags, the build pulls in C libraries (gpgme, btrfs, devicemapper) that aren't needed.

```bash
CGO_ENABLED=0 go build -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" -o picolet ./cmd/picolet
```

## Test

```bash
go test -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" ./... -race -count=1
```

## Lint

```bash
go vet -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" ./...
gofmt -l .
```

## Package Architecture

- `cmd/picolet` — CLI entry point (urfave/cli v3), defines `run`, `validate`, `resolve` commands
- `pkg/agent` — Main reconciliation loop: poll → resolve → reconcile → apply → health check
- `pkg/agentcfg` — Agent configuration loading from YAML
- `pkg/applier` — Phased applier: writes files, reloads systemd, restarts pods (file → systemd → podman)
- `pkg/config` — Fleet config loading: fleet.yml, assignments.yml, host.yml
- `pkg/gitpoll` — Git repository polling and clone/pull with change detection
- `pkg/health` — Post-apply health checking for managed systemd units
- `pkg/metrics` — Prometheus metrics (reconciliation counts, errors, durations)
- `pkg/reconciler` — Computes diff between desired state and current on-disk state
- `pkg/resolver` — Template rendering and file resolution per host
- `pkg/rollback` — Snapshot current state before apply, restore on failure
- `pkg/state` — Persistent state store (last applied commit, file hashes)
- `pkg/validator` — Validates quadlet files (via podman library), K8s manifests (via k8s.io/api), systemd units

## Key Dependencies

- `containers/podman/v5` — Quadlet validation (uses `quadlet.Convert*()` functions)
- `k8s.io/api` — Strict K8s manifest validation via real API types
- `go-git/go-git/v5` — Git clone/pull without shelling out
- `coreos/go-systemd/v22` — D-Bus systemd control
- `prometheus/client_golang` — Metrics exposition
- `urfave/cli/v3` — CLI framework
