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
| `manifests/<app>/` | `.yml` (Kubernetes resources only) | `/var/lib/picolet/manifests/<app>/` |
| `files/<app>/` | any | `/var/lib/picolet/files/<app>/` |
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
  manifests/    # K8s YAML only — validated against k8s.io/api types
  files/        # opaque, container-mounted files; validated only as YAML if .yml/.yaml
  picolet.yml
```

`manifests/` and `files/` may contain nested directories. The other six category directories
must contain files directly.

`picolet.yml` is optional service metadata. It does not deploy a resource by
itself; the bundle still needs at least one normal resource file.

Strict bundle rules:

| Rule | Behavior |
|------|----------|
| missing `services/<name>/` | error |
| `services/<name>/` exists but is not a directory | error |
| empty bundle | error |
| unknown entry at bundle root | error |
| category-named file at bundle root | error |
| nested directory under any category except `manifests/` and `files/` | error |
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

### Hooks

Service bundles can declare actions to run after assigned Podman secrets or
manifest files change. Put them in `services/<name>/picolet.yml` or
`services/<name>/picolet.yml.tmpl` (only one of the two — bundles containing
both are rejected). The example below uses Go template syntax, so it must be in
a `.tmpl` file:

```yaml
# services/<name>/picolet.yml.tmpl
hooks:
  - name: vmalert-rules
    secrets: [vmalert_rules]
    unit: vmalert.service
    action: http
    method: GET
    url: 'http://localhost:{{ index .Ports "vmalert" }}/vmalert/-/reload'
    health_url: 'http://localhost:{{ index .Ports "vmalert" }}/vmalert/health'

  - name: victoriametrics-scrape-reload
    files: [config/scrape.yml]
    unit: victoriametrics.service
    action: http
    method: GET
    url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/-/reload'
    health_url: 'http://localhost:{{ index .Ports "victoriametrics" }}/prometheus/health'
```

Hooks run after secret/manifest writes and before normal unit restarts. If
multiple changed secrets or manifests match one hook, Picolet runs that hook
once. If the unit is already scheduled for restart because its Quadlet changed,
Picolet skips reload hooks for that unit.

Hook names must be unique across all service bundles assigned to a host.

Each hook must specify at least one trigger — `secrets`, `manifests`, or `files`.
When more than one is set, the hook fires if ANY listed secret OR manifest OR file changed.

The `manifests` field uses paths relative to the service bundle's `manifests/`
directory (e.g., `app/deployment.yml`); use `manifests/` only for Kubernetes
resources fed to `podman kube play`. The `files` field uses paths relative to
the service bundle's `files/` directory (e.g., `config/scrape.yml`); use `files/`
for arbitrary container-mounted config (Prometheus scrape configs, vmalert rules,
etc.).

Supported actions:

| Action | Required fields | Behavior |
|--------|-----------------|----------|
| `http` | `unit`, `url`, at least one trigger (`secrets`, `manifests`, and/or `files`) | Send `method` (`POST` by default, `GET` also supported), then `GET health_url` when set |
| `signal` | `unit`, `container`, at least one trigger (`secrets`, `manifests`, and/or `files`) | Send `signal` (`HUP` by default) to the Podman container |
| `restart` | `unit`, at least one trigger (`secrets`, `manifests`, and/or `files`) | Restart the systemd unit after applying changes |

By default hook failures are non-fatal and keep the current process running:
`on_failure: keep_running`. Use `on_failure: restart` only when a failed reload
should fall back to a restart. Keeping the process running is usually safer for
config reload APIs that reject invalid config while continuing with the old
valid config.

Each retry attempt increments `picolet_reconciliation_total{result="retry_pending"}`
and emits an `apply incomplete` warning in the agent log; the hook is dropped
from the pending list once it succeeds, falls back to a restart, or exhausts
its `max_retries` budget.

For services whose config is mounted through Podman secrets, verify that the
running container sees replaced secret content on your target Podman version.
If not, use `action: restart`.

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
| `readOpSecret(ref)` | Resolve a 1Password reference, e.g. `op://vault/item/field` |
| `readProtonPassSecret(ref)` | Resolve a Proton Pass reference, e.g. `pass://share/item/field` |
| `manifestPath(relPath)` | Return the absolute deployed path for a manifest file (handles rootless/rootful automatically). `relPath` is relative to the service's `manifests/` dir |
| `filePath(relPath)` | Return the absolute deployed path for a file (handles rootless/rootful automatically). `relPath` is relative to the service's `files/` dir |
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

## Secret Providers

Picolet integrates with two secret managers so cleartext credentials never live in the fleet repo. Both providers can be configured at the same time; references are routed by URI scheme.

| Provider | Scheme | Underlying tool | Auth model |
|----------|--------|-----------------|------------|
| 1Password | `op://vault/item/field` | Official Go SDK | Service-account token |
| Proton Pass | `pass://share/item/field` | `pass-cli` (bundled in the container image) | Personal Access Token |

Refs can appear in two places:

- **Direct Podman secrets** in `assignments.yml` or `hosts/<name>/host.yml` under `secrets:` — Picolet resolves the value and creates a Podman secret named after the URI components (e.g. `vault_item_field` or `share_item_field`).
- **Inside templates** via `{{ readOpSecret "op://..." }}` or `{{ readProtonPassSecret "pass://..." }}` — Picolet collects all calls in a first render pass, batches them per provider, then renders again with the resolved values.

### 1Password setup

```yaml
# /etc/picolet/config.yml
onepassword:
  token_path: /etc/picolet/secrets/op-service-account-token  # required
  token_expires_at: 2026-12-31T23:59:59Z   # optional but recommended (RFC3339)
  refresh_interval: 6h        # default 6h, minimum 1m
  git_token_ref: op://Infra/picolet-git/token  # optional: resolve git PAT via 1Password
```

### Proton Pass setup

The container image ships the official `pass-cli` binary (multi-arch, version pinned and SHA-256-verified at build time). On a host without picolet's container, install it manually following https://protonpass.github.io/pass-cli/.

For unattended use (recommended in containers):

```yaml
# /etc/picolet/config.yml
protonpass:
  pat_path: /etc/picolet/secrets/pp-pat              # PAT format: pst_…::TOKENKEY
  pat_expires_at: 2026-09-15T00:00:00Z                # optional but recommended (RFC3339)
  session_dir: /var/lib/picolet/protonpass/.session  # optional in PAT mode; this is the default
  refresh_interval: 6h                                # default 6h, minimum 1m
  git_token_ref: pass://abc.../item.../token          # optional: resolve git PAT via Proton Pass
```

In PAT mode, picolet selects `pass-cli`'s filesystem-based local key provider (`PROTON_PASS_KEY_PROVIDER=fs`). `pass-cli` auto-generates and rotates a `local.key` inside `session_dir`, so there is nothing for the operator to provision beyond the PAT itself. A backup of `session_dir` restored to a different host will produce an unreadable session and force a re-login on next start — this is a session-invalidation failure, not a credential exposure.

`token_expires_at` / `pat_expires_at` are surfaced as `picolet_secret_credential_expires_at{provider}` so you can alert before the credential lapses — see `docs/alerting.md` for the recommended rule. pass-cli does not expose PAT expiry programmatically, so this value must be entered manually at provisioning time and bumped on every rotation.

For local development, leave `pat_path` empty (Lazy mode). Picolet then uses any pre-existing `pass-cli login` session in your home directory and never overwrites it. Run `pass-cli test` to verify the same session check Picolet runs at startup.

Find share IDs and item IDs with:

```bash
pass-cli vault list
pass-cli item list --share-id <share-id>
```

### Limitations and operational notes

- **PAT expiry.** Personal Access Tokens have a mandatory expiration. Rotate before they expire and `systemctl restart picolet` to pick up the new token.
- **Online only.** Each reconcile that touches a `pass://` ref needs HTTPS to Proton.
- **Per-vault PATs.** A single PAT scopes to the vaults granted to it in Proton Pass. Make sure every `pass://` ref used by a host is accessible to the configured PAT.
- **Architecture.** `pass-cli` requires 64-bit Linux (`linux/amd64` or `linux/arm64`); 32-bit RPi OS is not supported.
- **Session directory exemption.** In PAT mode, `protonpass.session_dir` defaults to `/var/lib/picolet/protonpass/.session`. It is owned by `pass-cli`, not by picolet, and the orphan-cleanup scanner does not touch it.
- **Mutual exclusion.** Each authentication resource (`git_token`, GitHub App credentials) can be supplied by exactly one source — direct config, 1Password, or Proton Pass. Mixing for the same resource is rejected at startup.
