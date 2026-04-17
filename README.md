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

## Deployment

Picolet manages itself via GitOps. Bootstrap gets it running; after that, the fleet git repo controls everything — including picolet's own version.

### 1. Create a Fleet Repository

Use `deploy/fleet-repo/` as a starting point. Your fleet repo needs:

- `fleet.yml` — image versions and ports
- `assignments.yml` — which files go to which hosts
- `hosts/<hostname>/host.yml` — per-host config
- `quadlets/containers/picolet.container.tmpl` — picolet's own Quadlet

See `deploy/fleet-repo/` for a complete example.

### 2. Bootstrap a Host

#### Rootful (production)

```bash
# 1. Install Podman
sudo apt install podman

# 2. Create agent config
sudo mkdir -p /etc/picolet/secrets
sudo tee /etc/picolet/config.yml << EOF
hostname: "my-pi"
repo_url: "https://github.com/yourorg/fleet.git"
git_token_path: "/etc/picolet/secrets/git_token"
EOF
echo "ghp_yourtoken" | sudo tee /etc/picolet/secrets/git_token > /dev/null
sudo chmod 600 /etc/picolet/config.yml /etc/picolet/secrets/git_token

# 3. Run bootstrap
sudo bash deploy/bootstrap/bootstrap.sh
```

#### Rootless (dev/test)

```bash
# 1. Create agent config
mkdir -p ~/.config/picolet/secrets
cat > ~/.config/picolet/config.yml << EOF
hostname: "my-pi"
repo_url: "https://github.com/yourorg/fleet.git"
systemd_user: true
git_token_path: "/etc/picolet/secrets/git_token"
EOF
echo "ghp_yourtoken" > ~/.config/picolet/secrets/git_token
chmod 600 ~/.config/picolet/config.yml ~/.config/picolet/secrets/git_token

# 2. Run bootstrap (no sudo)
bash deploy/bootstrap/bootstrap-rootless.sh
```

### 3. What Happens Next

1. Picolet starts and clones your fleet repo
2. First reconcile: picolet replaces the bootstrap container file with the fleet template version → **one-time self-restart** (expected)
3. After restart: picolet is fully self-managed via GitOps

### 4. Updating Picolet

Bump the image version in your fleet repo's `fleet.yml`:

```yaml
images:
  picolet: "ghcr.io/schjan/picolet:v0.2.0"  # was v0.1.0
```

Push to git. Picolet detects the change, writes the updated Quadlet, and restarts itself with the new image.

### 5. Monitoring

```bash
# Logs (rootful / rootless)
journalctl -fu picolet.service
journalctl --user -fu picolet.service

# Prometheus metrics
curl http://localhost:9417/metrics

# Health check
curl http://localhost:9417/health
```

## Fleet Repository Reference

Your fleet repo controls what picolet deploys. See `deploy/fleet-repo/` for a complete example.

### Config Files

| File | Purpose |
|------|---------|
| `fleet.yml` | Image versions, ports, shared config (Renovate-managed) |
| `assignments.yml` | Maps pi_type + features to file sets per host |
| `hosts/<name>/host.yml` | Per-host config: hostname, type, features, secrets |

### File Categories

| Directory | Extension | Deploys to |
|-----------|-----------|------------|
| `quadlets/networks/` | `.network` | `/etc/containers/systemd/picolet/` |
| `quadlets/volumes/` | `.volume` | `/etc/containers/systemd/picolet/` |
| `quadlets/containers/` | `.container` | `/etc/containers/systemd/picolet/` |
| `quadlets/kube/` | `.kube` | `/etc/containers/systemd/picolet/` |
| `manifests/<app>/` | `.yml` | `/var/lib/picolet/manifests/<app>/` |
| `secrets/` | `.yml` | Podman secrets |
| `systemd/` | `.socket` | `/etc/systemd/system/` |

### Service Bundles

Use `services:` in `assignments.yml` when one logical service spans several file
categories. A bundle expands into the same per-category files Picolet already
understands, so bundles and legacy explicit lists can coexist in the same repo.

Bundle layout is typed by directory name. Only create the category directories a
service actually uses.

```text
services/<name>/
  containers/
  volumes/
  networks/
  kube/
  systemd/
  secrets/
  manifests/
```

`manifests/` may contain nested directories. The other six category directories
must contain files directly.

Strict bundle rules:

| Rule | Behavior |
|------|----------|
| missing `services/<name>/` | error |
| `services/<name>/` exists but is not a directory | error |
| empty bundle | error |
| unknown entry at bundle root | error |
| category-named file at bundle root | error |
| nested directory under non-`manifests/` category | error |
| two sources resolving to the same destination | error |

Dotfiles and loose files are not special-cased. Keep the bundle directory clean
or ignore those files at the repo level before they land in the fleet repo.

Bundled manifests keep their real repo path for template rendering, but Picolet
strips the `services/<name>/` prefix when deriving the deployed destination. For
example, `services/web/manifests/app/deployment.yml.tmpl` deploys to
`/var/lib/picolet/manifests/app/deployment.yml`.

Collision detection happens during `resolve` / `validate`. Picolet rejects:

- quadlet files that would overwrite another file in the shared quadlet directory
- manifest files that normalize to the same deployed path
- secrets that normalize to the same `secret:<name>` destination, such as
  `foo.yml` and `foo.yaml`

To migrate an explicit service, create `services/<name>/<category>/` directories,
move the files without renaming them, and replace the per-category lists in
`assignments.yml` with `services: [<name>]`.

**The cutover must be atomic per service.** The same file listed under both the
legacy paths and a `services:` bundle resolves to the same on-disk destination,
so Picolet fails the reconciliation with a destination collision. Remove the
legacy paths in the same commit that introduces `services: [<name>]`.

### Templates

Files ending in `.tmpl` are rendered with Go `text/template` (`missingkey=error`) plus Sprig's hermetic text helpers. Static files are deployed as-is.

Available data: `.Host` (hostname, pi_type, features), `.Images`, `.Ports`, `.Fleet` (all hosts + config).

| Function | Purpose |
|----------|---------|
| `readFile(path)` | Embed a static file from the repo |
| `renderTemplate(name, data)` | Render another template inline |
| `glob(patterns...)` | Resolve one or more glob patterns (strict: invalid/empty matches are errors), sorted + deduplicated |
| `concatFiles(patterns...)` | Read matched files raw and concatenate them with newline glue only when needed |
| `indent(n, str)` | Indent all non-empty lines by n spaces |
| `nindent(n, str)` | Prepend a newline, then indent all non-empty lines by n spaces |
| `readSecretFile(path)` | Read secret (placeholder in CI mode) |
| `has(item, slice)` | Sprig: check if a value is present in a list |

Use this when runtime expects one file but you want many repo fragments. Example:

```yaml
groups:{{ concatFiles "rules/vmalert/*.yml" | nindent 2 }}
```

Keep fragments unindented (`- name: ...`) and let the template handle indentation with `nindent`.

`concatFiles` is intentionally raw-only (it does not auto-render matched `.tmpl` files). If you need rendered fragments, use `glob`, iterate, and call `renderTemplate` explicitly in your template.

### Validation

All files are validated before deployment: quadlet files via Podman's own `quadlet.Convert*()`, K8s manifests via strict unmarshalling into `k8s.io/api` types, systemd units structurally, and templates at render time (`missingkey=error`).

Secrets always require non-empty content. Repo-backed YAML secrets (`.yml` / `.yaml`, including `.tmpl`) are also syntax-validated after template rendering. External placeholder secrets in repo-only validation mode are skipped for YAML syntax checks.

Run `./picolet validate` in CI to catch errors before pushing.
