# picolet

**picolet** = **pico** (smaller-than-nano, as in tiny) + **quadlet** -- a tiny Quadlet manager.

A minimal, single-binary GitOps agent for managing Podman Quadlet files on Raspberry Pi fleets. Think Flux/ArgoCD, but for hosts running Podman instead of Kubernetes.

## Installation

### Binary (GitHub Releases)

Download the latest release for your platform from [GitHub Releases](https://github.com/schjan/picolet/releases).

### Container (GHCR)

```bash
docker pull ghcr.io/schjan/picolet:latest
```

## Quick Start

### Build

Requires [Task](https://taskfile.dev/) (`go install github.com/go-task/task/v3/cmd/task@latest`).

```bash
task build          # native binary
task build-arm64    # cross-compile for RPi
task test           # run tests
task lint           # go vet + gofmt
```

### Validate & Resolve

The `validate` and `resolve` commands require a fleet repository with `fleet.yml`, `assignments.yml`, and host configs. See the fleet repo for details.

```bash
./picolet validate
./picolet resolve --host=rpi5-1
```

### Build Tags

Picolet uses the Podman Go bindings (`pkg/bindings`) as a pure socket client. The following build tags are required (centralised in `Taskfile.yml`):

| Tag | Purpose |
|-----|---------|
| `remote` | Exclude local libpod engine code — picolet only talks to Podman over the socket API |
| `containers_image_openpgp` | Use pure-Go OpenPGP instead of gpgme (C library) |
| `exclude_graphdriver_btrfs` | Skip btrfs graph driver (C library) |
| `btrfs_noversion` | Skip btrfs version check |
| `exclude_graphdriver_devicemapper` | Skip devicemapper graph driver (C library) |

These tags are also set in the `Containerfile` and `.github/workflows/ci.yml`.

## Architecture

### Config Files (in fleet repo)

| File | Purpose |
|------|---------|
| `fleet.yml` | Image versions, ports, shared config (Renovate-managed) |
| `assignments.yml` | Maps pi_type + features to file sets per host |
| `hosts/<name>/host.yml` | Per-host config: hostname, type, features, secrets |

### Template Processing

Files ending in `.tmpl` are processed by Go `text/template` with `missingkey=error`. Static files are deployed as-is.

Template data available as `.`:

- `.Host.Hostname`, `.Host.AnsibleHost`, `.Host.PiType`
- `.Host.AlloyMode` (`"agent"` or `"gateway"`)
- `.Host.IsGateway`, `.Host.HasMosquitto`
- `.Images` (map of image names to full refs)
- `.Ports` (map of port names to numbers)
- `.Fleet.Config.Prometheus.*` (Prometheus config)
- `.Fleet.Hosts` (all hosts, for fleet-aware templates)

Custom template functions:

| Function | Purpose |
|----------|---------|
| `readFile(path)` | Embed a static file from the repo |
| `renderTemplate(name, data)` | Render another template inline |
| `indent(n, str)` | Indent all lines by n spaces |
| `readSecretFile(path)` | Read secret (placeholder in CI mode) |

### File Categories

| Directory | Extension | Deploys to |
|-----------|-----------|------------|
| `quadlets/networks/` | `.network` | `/etc/containers/systemd/` |
| `quadlets/volumes/` | `.volume` | `/etc/containers/systemd/` |
| `quadlets/containers/` | `.container` | `/etc/containers/systemd/` |
| `quadlets/kube/` | `.kube` | `/etc/containers/systemd/` |
| `manifests/<app>/` | `.yml` | `/var/lib/picolet/manifests/<app>/` |
| `secrets/` | `.yml` | Podman secrets |
| `systemd/` | `.socket` | `/etc/systemd/system/` |

### Validation

- **Quadlet files**: Validated using Podman's own `quadlet.Convert*()` functions (same code as `podman-system-generator`), including cross-reference resolution between units (e.g., a `.container` referencing a `.network`)
- **K8s manifests**: Strict unmarshalling into real `k8s.io/api` types (`Deployment`, `DaemonSet`, `Pod`, `ConfigMap`, `Secret`, `PersistentVolumeClaim`) — catches unknown fields, wrong types, structural issues
- **Systemd units**: Section header presence
- **Templates**: `missingkey=error` catches undefined variables at render time

## Project Structure

```
cmd/picolet/main.go        CLI entry point (urfave/cli v3)
pkg/
  agent/                    Main reconciliation loop orchestrator
  agentcfg/                 Agent configuration loading
  applier/                  Phased file + systemd + podman applier
  config/                   Config loading (fleet, hosts, assignments)
  gitpoll/                  Git repository polling
  health/                   Health checking for managed units
  metrics/                  Prometheus metrics
  reconciler/               Desired vs current state diffing
  resolver/                 Template rendering + file resolution
  rollback/                 Snapshot and restore on failure
  state/                    Persistent reconciliation state
  validator/                Quadlet + manifest validation
```
