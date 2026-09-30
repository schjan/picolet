# picolet

**picolet** = **pico** (smaller-than-nano, as in tiny) + **quadlet** -- a tiny Quadlet manager.

A minimal, single-binary GitOps agent for managing Podman Quadlet files on hosts running Podman — a Raspberry Pi, a VPS or any other Linux machine. Think Flux/ArgoCD, but for Podman instead of Kubernetes.

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
./picolet resolve --host=srv-1
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

Use `deploy/fleet-repo/` as a starting point: a worked example of one Machine (`srv-1`) with three Hosts — the default user (`srv-1`), the rootful Host (`srv-1-system`) and an isolated user (`srv-1-runner`); see [Machines, users and ports](#machines-users-and-ports). Your fleet repo needs:

- `fleet.yml` — image versions and ports
- `assignments.yml` — which files go to which hosts
- `hosts/<hostname>/host.yml` — per-host config
- `services/picolet-system/` (rootful) or `services/picolet/` (rootless) — picolet's own Service Bundle: its Quadlet and its config secret

Read [Fleet conventions](#fleet-conventions) before you lay the repository out.

### 2. Bootstrap a Host

#### Rootful (production)

```bash
# 1. Install Podman
sudo apt install podman

# 2. Create agent config
sudo mkdir -p /etc/picolet/secrets
sudo tee /etc/picolet/config.yml << EOF
hostname: "srv-1-system"
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
hostname: "srv-1"
repo_url: "https://github.com/yourorg/fleet.git"
systemd_user: true
git_token_path: "/etc/picolet/secrets/git_token"
EOF
echo "ghp_yourtoken" > ~/.config/picolet/secrets/git_token
chmod 600 ~/.config/picolet/config.yml ~/.config/picolet/secrets/git_token

# 2. Run bootstrap (no sudo)
bash deploy/bootstrap/bootstrap-rootless.sh
```

#### Containerized picolet & `host_data_dir`

The `rootless` flag describes picolet's **internal** assumptions — the path layout
it uses (`/etc` + `/var/lib` vs `~/.config` + `~/.local/share`) and, by default,
which systemd instance it talks to (`systemd_user`). It does **not** describe the
host deployment model.

picolet itself usually runs as a container. When its volume mounts are
*asymmetric* — the host sees a directory at a different path than picolet does,
e.g. `Volume=%h/.local/share/picolet:/var/lib/picolet` — picolet writes files
correctly (the mount lands them) but the `filePath`/`manifestPath` template
helpers would bake picolet's *internal* path into rendered quadlet `Volume=` lines,
which the **host's** podman cannot resolve.

Set `host_data_dir` to the host-visible path so those helpers emit
host-resolvable strings:

```yaml
# config.yml — picolet runs containerized, host data dir mounted at a different path
host_data_dir: /home/app/.local/share/picolet
```

`host_data_dir` only changes the path string templates emit; it does not change
where picolet writes files (that stays the internal data dir, distinct from the
unrelated `data_dir` option which overrides picolet's own repo/state/lock dir).
The first reconcile after setting it re-renders affected quadlets, causing a
one-time restart of the units that reference `filePath`/`manifestPath` paths.

> `picolet resolve` / `picolet validate` (run without `--config`) cannot read
> `host_data_dir` and will preview picolet's internal paths.

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

The agent serves `/metrics`, `/health`, `/webhook` and the dashboard on
**`127.0.0.1:9417`** by default. None of them is authenticated, so a fresh
install exposes nothing on a public Machine; Prometheus scrapes it from the same
Machine, and remote access goes through a reverse proxy or a mesh network.

```bash
# Logs (rootful / rootless)
journalctl -fu picolet.service
journalctl --user -fu picolet.service

# Prometheus metrics
curl http://127.0.0.1:9417/metrics

# Health check
curl http://127.0.0.1:9417/health
```

A Prometheus (or VictoriaMetrics) instance on the same Machine scrapes loopback;
two Agents on one Machine differ only in port (a Fleet template can generate
this list with the `siblings` helper, see
[Machines, users and ports](#machines-users-and-ports)):

```yaml
scrape_configs:
  - job_name: picolet
    static_configs:
      - targets: ["127.0.0.1:9417", "127.0.0.1:9418"]
```

Each Agent names itself so sibling Agents stay apart: `picolet_host_info`
carries `role`, `machine` and `user` labels (`user="root"` for the rootful
Host, so the label is never empty), the dashboard header shows
`machine / user` next to the hostname, and `picolet resolve --host` opens with
a `# host=… role=… machine=… user=… listen_port=…` line. Outside the metric,
`user` is `host.yml`'s `user:` as written: the rootful Host's dashboard header
shows only its Machine, and its resolve line reads `user=`.

```promql
# Reconciliations per Machine, split by Agent user
sum by (machine, user) (
  rate(picolet_reconciliation_total[1h])
  * on(instance) group_left(machine, user) picolet_host_info
)
```

To expose the listener deliberately, set `listen_addr` in the agent config:

```yaml
listen_addr: "0.0.0.0:9417"   # every interface
listen_addr: "192.168.1.20:9417"  # one interface
listen_addr: "127.0.0.1:9418"     # loopback, second Agent on the same Machine
```

`metrics_port` remains the short form for the port and keeps the loopback
default. Setting both is only allowed when they name the same port — otherwise
the agent refuses to start.

A **containerized** agent reaches the Machine's loopback only with
`Network=host` (both reference Quadlets in `deploy/fleet-repo/` use it). In a
container with its own network namespace, bind `0.0.0.0` and publish the port
instead; picolet logs a warning when it detects that combination.

### 6. Node Maintenance (image pruning)

Long-running nodes accumulate unused container images as picolet rolls out new
image tags. To reclaim that space, the agent periodically removes **unused
container images** — every image not referenced by any container (running or
stopped), equivalent to `podman image prune -a`. It never touches volumes,
networks, or images currently in use.

The prune runs **inside the reconcile loop**, so it is strictly serialized with
picolet's own image pulls and can never delete an image mid-deployment.

Configured in `/etc/picolet/config.yml` (defaults shown):

```yaml
prune_images: true       # set false to disable pruning
prune_interval: "168h"   # weekly; a non-zero value below 1m is rejected
```

Defaults: **enabled, weekly**. To disable pruning, set `prune_images: false` —
an unset or `0s` `prune_interval` falls back to the weekly default rather than
disabling it. Observability:

```bash
journalctl -u picolet | grep "image prune"
curl -s http://127.0.0.1:9417/metrics | grep image_prune
# picolet_image_prune_total{result="success"}, picolet_images_pruned_total,
# picolet_image_prune_reclaimed_bytes_total, picolet_last_image_prune_timestamp
```

Notes:
- On a **fresh node** (empty state) the first reconcile prunes immediately.
- On a **mixed host**, `prune -a` also removes images that belong to non-picolet
  workloads if no container references them. On a dedicated fleet host this is
  the intended behavior.

## Fleet Repository Reference

Your fleet repo controls what picolet deploys. See `deploy/fleet-repo/` for a complete example.

### Config Files

| File | Purpose |
|------|---------|
| `fleet.yml` | Image versions and ports (Renovate-managed) |
| `assignments.yml` | Assigns files (`paths:`), Podman secrets (`secrets:`) and Service Bundles (`services:`) to hosts: `base`, then per role, then per feature |
| `hosts/<name>/host.yml` | Per-host config: hostname, external hostname, role, features, machine, user, listen_port |

### Machines, users and ports

A **Machine** runs one or more **Hosts**, each served by its own Agent under its
own Linux user. `host.yml` says where a Host runs:

| Key | Default | Meaning |
|-----|---------|---------|
| `machine:` | the `hostname` | The Machine this Host runs on; a hostname label, compared case-insensitively (`VPS-1` and `vps-1` are one Machine) |
| `user:` | absent = rootful | The Linux user the Agent runs as; omit it for the rootful Host (`user: root` is rejected) |
| `listen_port:` | `fleet.yml` `ports.picolet_metrics` when `user:` is set, `ports.picolet_system_metrics` for the rootful Host | The port the Agent listens on |

```yaml
# hosts/vps-1-runner/host.yml
hostname: vps-1-runner
role: runner
machine: vps-1
user: runner
listen_port: 9419
```

The Fleet is the sole owner of the Agent's listen address: the reference Agent
config templates render `listen_addr: 127.0.0.1:{{ .Host.ListenPort }}`, and a
Host with no `listen_port:` whose default `ports` key is missing fails to load.

Loading the Fleet rejects (a collision names both Hosts):

- two host directories declaring the same `hostname`;
- two Hosts on one Machine with the same `user:` (including two rootful Hosts);
- two Hosts on one Machine with the same effective listen port;
- an invalid `user:` (lowercase letters, digits, `_`, `-`; not starting with a
  digit or `-`; at most 32 characters), a `machine:` — declared or defaulted from
  the hostname — that is not a hostname label, or a `listen_port:`
  outside 1–65535.

Host names follow a convention — `<machine>` for the Machine's default user
(the default user is named after the Machine), `<machine>-system` for the rootful
Host, `<machine>-<user>` for further users — that picolet never parses; only
`machine:` and `user:` count.

Templates see the topology on `.Host` and every `.Fleet.Hosts` entry (see
[Templates](#templates)); `siblings` lists the other Hosts on the same Machine,
e.g. to scrape their Agents over loopback
(`testdata/example-fleet/services/agent-scrape/`):

```yaml
      - targets: ["127.0.0.1:{{ .Host.ListenPort }}"]
{{- range siblings }}
      - targets: ["127.0.0.1:{{ .ListenPort }}"]
{{- end }}
```

**Upgrading.** Every Agent parses every `host.yml`. Agents from this release on
log a warning for a key they do not know and carry on (`picolet validate` still
fails on it, so typos break CI), but older Agents stop loading the whole Fleet
on any new key. Roll out in this order:

1. Add `picolet_metrics` / `picolet_system_metrics` to `fleet.yml` `ports` — the
   only change that is safe for older Agents, and required by new ones.
2. Upgrade the picolet image on **all** Hosts.
3. Only then add `machine:`, `user:` or `listen_port:` to any `host.yml`.

Two cases in step 3 are required, not optional: a rootless Host needs `user:`
(without it the Host counts as rootful and takes `picolet_system_metrics`), and
a Host whose `hostname` is not a hostname label (e.g. contains `.` or `_`) needs
`machine:` — until it has one, upgraded Agents refuse to load the Fleet, so
commit it right after step 2 completes.

### Fleet conventions

`deploy/fleet-repo/` follows these conventions; they are not enforced by
picolet unless stated. It is one Machine, `srv-1`, with three Hosts — `srv-1`
(default user `app`), `srv-1-system` (rootful) and `srv-1-runner` (user
`runner`) — plus a `metrics` Service Bundle, all under the generic domain
`example.net`.

- **Host naming.** See [Machines, users and ports](#machines-users-and-ports):
  `srv-1`, `srv-1-system` and `srv-1-runner` follow it.
- **Nothing secret in git.** The Fleet repo holds templates and references
  (`op://`, `pass://`), never values: no tokens, passwords, private keys or
  key-bearing URLs, and no file a secret was ever committed to. Real values
  live in a [Secret Provider](#secret-providers) or in the Host's local
  secrets directory.
- **One provider token per Machine.** By default every Agent on a Machine uses
  the same provider token: one credential to provision and rotate per Machine.
  The stricter options are a token scoped to a single Host's vault or share, or
  no provider at all (secrets provisioned on the Host by hand) — pick them
  when a Host, such as an isolated `runner` user, must not be able to read what
  its siblings can.
- **Nothing the Agent deploys hosts the Fleet.** The Fleet repository (forge,
  git server) and the secret provider must never run on a service that an Agent
  deploys from that same Fleet: if that service is down or broken by a bad
  commit, no Agent can fetch the fix or the credentials to repair it.
- **Cross-Host traffic uses loopback ports or the public hostname.** Hosts on
  one Machine talk to each other over `127.0.0.1:<port>` (each Host's ports come
  from the Fleet, e.g. `siblings` → `.ListenPort`); Hosts on different Machines
  use the public hostname (`external_hostname`, e.g. `srv-1.example.net`), never
  a private or overlay address that only some Hosts can reach.
- **The network story.**
  - The Agent and the metrics stack stay `Network=host`, so they reach the
    Machine's loopback and the other Agents.
  - Services use one `.network` per Service Bundle and publish their metrics on
    `127.0.0.1:<port>`, so the metrics stack scrapes them over loopback.
  - The reverse proxy takes ports 80 and 443 through `.socket` units and
    publishes no port at all.
- **Scrape snippets stay out of the bundles.** A bundle's files are delivered to
  every Host that carries it, so a bundle's scrape jobs live at the Fleet root as
  `scrape/<bundle>.yml.tmpl` - registered as a template, never delivered, and
  collision-free because the file is named after its bundle. The metrics
  bundle's `prometheus.yml.tmpl` composes the snippets of the bundles assigned
  to its Host.
- **Never copy a live database.** Back up with the database's own dump or
  snapshot tool, or stop the service first; a file-level copy of a running
  database is not a backup.
- **Validate in CI with the exact image the Fleet deploys.** Run
  `picolet validate` in CI with the `images.picolet` reference from `fleet.yml`,
  so validation sees the same Podman `quadlet.Convert*()` as the Agents:

  ```bash
  podman run --rm -v "$PWD:/fleet:ro" ghcr.io/schjan/picolet:v0.1.0 validate --repo-dir /fleet
  ```

  (`v0.1.0` is a placeholder for the tag in your `fleet.yml`.)

**Keeping a public Fleet generic.** Use placeholders such as `srv-1`,
`example.net` and `example/fleet` in anything you publish. `deploy/fleet-repo/`
is guarded by a CI denylist grep: `.github/fleet-denylist.txt` holds generic
patterns (one per line, `#` comments allowed) and the `FLEET_DENYLIST` Actions
secret holds the real machine names, domains and tailnet suffixes,
newline-separated, so they never appear in the repository. The check is
case-insensitive and fixed-string over `deploy/fleet-repo/`; without the secret
(fork PRs) only the file applies.

### File Categories

Picolet derives each file's category from its path (the full rule is under
[`paths:` entries](#paths-entries)): a first-level `manifests/`, `files/` or `secrets/`
directory wins, otherwise the extension decides. Outside those three, which directory
a unit sits in is up to you.

| Path or extension | Category | Deploys to |
|-------------------|----------|------------|
| any, below a first-level `manifests/` | Kubernetes manifest (`.yml`, Kubernetes resources only) | `/var/lib/picolet/manifests/<path below manifests/>` (rootful) or `~/.local/share/picolet/manifests/…` (rootless) |
| any, below a first-level `files/` | opaque File | `/var/lib/picolet/files/<path below files/>` (rootful) or `~/.local/share/picolet/files/…` (rootless) |
| any, below a first-level `secrets/`, or listed under `secrets:` | Podman secret | Podman secrets |
| `.network` `.volume` `.container` `.kube` `.pod` `.image` `.build` | Quadlet unit | `/etc/containers/systemd/picolet/` (rootful) or `~/.config/containers/systemd/picolet/` (rootless) |
| `.service` `.timer` `.socket` `.target` `.path` | systemd unit | `/etc/systemd/system/` (rootful) or `~/.config/systemd/user/` (rootless) |

`.artifact` is known to Podman but not deployable yet: `validate` rejects it.

A new Quadlet type is a table row in `pkg/config/categories.go`, never an
`assignments.yml` schema change — see
[ADR 0001: Quadlet is the config](docs/adr/0001-quadlet-is-the-config.md).

A `.pod` generates `<name>-pod.service` (hooks may target it as `unit: <name>.pod`).
Containers join it with `Pod=<name>.pod`; the pod must be deployed to the same host.
The pod service starts its members (unless `StartWithPod=false`), so when a pod
changes in the same reconciliation as its members, only the pod service is restarted.
The agent's own container (`picolet.service`/`picolet-system.service`) must not join a
pod: `validate` rejects it, because restarting or stopping the pod would stop the agent.

A `.image` generates `<name>-image.service`, a `.build` `<name>-build.service` (hooks may
target them as `unit: <name>.image` / `unit: <name>.build`). Containers use them with
`Image=<name>.image` / `Image=<name>.build`; the unit must be deployed to the same host.
Deliver a build's Containerfile under `files/` and point at it with
`File={{ filePath "<app>/Containerfile" }}` plus `SetWorkingDirectory=file` (build context =
the Containerfile's directory). `validate` (and the agent, before applying) rejects a
`.build` whose Containerfile (`File=`), build context (`SetWorkingDirectory=` path) or working
directory (`[Service] WorkingDirectory=`) lies in the host's `files/` or `manifests/` data
directory but is not delivered by the host's assignments; a directory counts as delivered
when a delivered file lies below it. `podman build` looks for `File=` as written (a relative
one in `[Service] WorkingDirectory=`), then inside the build context, so it is rejected only
when every place it could be is in those data directories and none is delivered; a relative
`File=` without `[Service] WorkingDirectory=` is not checked, because the service's default
directory is the operator's. Absolute paths elsewhere (managed on the host), URLs, systemd
specifiers and paths relative to the unit file are not checked. Both generated services are
one-shots their consumers pull in: reported; the health loop never restarts one that fails
on its own, but retries an apply-time restart of a `.image` that failed (a changed `.image`
is pulled again; if the pull fails, it is pending like any failed unit restart, see
[Hooks](#hooks)).

#### Rebuild trigger

Podman watches nothing: a build (no `RemainAfterExit`) runs whenever a consumer starts,
and never because its inputs changed. Picolet therefore **rebuilds on change**, a
deliberate exception to the rule that one-shots systemd activates are never started by
Picolet. When a Reconciliation creates or updates a `.build`, or creates, updates or
deletes a file it builds from — its `File=`, or anything under its build context
directory — Picolet, after `daemon-reload`:

1. **starts** `<name>-build.service` and waits for the build (up to 30 minutes), before
   restarting anything else. A start, not a restart: systemd propagates a restart of a
   `Requires=` dependency to its consumers, stopping them before the build has succeeded.
2. **restarts** each running consumer — every unit that `Requires=` the build service
   (`Image=<name>.build` containers and volumes) — with job mode `ignore-dependencies`,
   after the other changed units, so the build is not run again: a running consumer is
   never stopped by a build. A consumer that is not running (one whose start failed on
   an earlier bad build) is started the ordinary way, with its dependencies; that runs
   the build again, as a full cache hit. Also left to the ordinary path: members of a pod
   restarted in the same Reconciliation (the pod restarts them, running the build again
   from cache). One-shots a timer runs are left alone (the next run uses the new image).
   A restart hook on the build service is covered by the build that already ran.

A **failed build fails the Reconciliation**: nothing has been restarted, the deployed files
are rolled back, and the commit counts toward the failed-commit gate like any failed apply.
Every tag the Reconciliation's builds write is put back on the image it named before
(including one an earlier, successful build of the same Reconciliation moved), so the
consumer keeps running the previous image and no image built from rolled-back inputs
stays tagged. The build service is left `failed` (reported, not retried by the health
loop). A build Picolet gives up waiting on (timeout, shutdown) is killed and waited for.
A failed consumer restart after a successful build is pending and retried like any
failed unit restart; `picolet apply` fails instead and saves no state, so the next apply
retries.

**Build runs after the rebuild.** Podman runs the build again whenever a consumer starts:
on the ordinary paths above, and on every later start (a reboot, a health-loop restart).
That re-run uses cached layers and executes no `RUN` step, and with the default
`Pull=missing` it does not contact the registry while the base image is present. Keep the
default on a `.build`: with `Pull=always` or `Pull=newer` every re-run checks the registry,
and fails while it is unreachable, which stops a consumer that had to be started anyway
(a pod or dependency that changed in the same commit, a reboot). Picolet checks a build
once, before restarting anything; it does not prevent these later runs.

The build context follows Quadlet: a `[Service] WorkingDirectory=`, unless
`SetWorkingDirectory=` is an absolute path; otherwise `SetWorkingDirectory=file` → the
Containerfile's directory, `=unit` → the Quadlet directory, a path → that directory
(relative to the Quadlet directory). Only local paths count: a URL or specifier (`%h`)
triggers nothing, and with neither key only `File=` does. A Reconciliation that touches
none of a build's inputs restarts neither the build nor its consumers. Do not set
`RemainAfterExit=yes` on a `.build`: the service would stay active and the start would
not rebuild.

#### `paths:` entries

Any assignment group (`base`, `roles`, `features`) may list files or directories
under `paths:` (Fleet-root-relative); picolet derives each file's category from its
path. Directories are expanded recursively.

1. A final `.tmpl` on a file name is stripped first (`web.pod.tmpl` is a pod template).
2. A first path segment `manifests/`, `files/` or `secrets/` selects that category
   (Kubernetes manifest, opaque File, Podman secret). The segment anywhere else, or
   two of them in one path, is an error.
3. Otherwise the extension decides: Quadlet extensions go to the Quadlet directory,
   systemd extensions to the systemd directory.
4. Any other file — unknown extension or none (`Containerfile`, `README.md`,
   `.gitkeep`) — is an error: move it under `files/` or remove it from the listed
   directory. `picolet.yml`/`picolet.yml.tmpl` are skipped.

Entries are Fleet-root-relative; `..` is rejected. A symlink below a listed directory
is an error (directories are walked without following links); a symlinked file listed
directly is read like any source. No symlink may point outside the Fleet repo.

```yaml
roles:
  worker:
    paths:
      - files/                       # every File below files/
      - quadlets/pods/shop.pod.tmpl  # one pod
```

A group accepts exactly `paths:`, `secrets:` and `services:`. `secrets:` lists files
(or `op://`/`pass://` refs, see [Secret Providers](#secret-providers)) deployed as
Podman secrets whatever their path; `services:` lists [Service Bundles](#service-bundles).
A file reached twice in one category deploys once; two sources for one destination
are an error. A file listed under `secrets:` that `paths:` also reaches as another
category deploys to both destinations (e.g. `secrets: [files/token]` with
`paths: [files/token]`).

### Service Bundles

Use `services:` in `assignments.yml` when one logical service spans several file
categories. A bundle `services/<name>/` is a plain directory, expanded exactly like
a directory listed under [`paths:`](#paths-entries) with paths taken relative to the
bundle: a unit's extension decides its category, and the bundle's `manifests/`,
`files/` and `secrets/` hold its manifests, Files and Podman secrets. Arrange
everything else however you like. Bundles and `paths:` coexist in one repo.

```text
services/<name>/
  web.network
  app/
    web.container.tmpl
    worker/worker.container
  secrets/      # Podman secrets
  manifests/    # K8s YAML only — validated against k8s.io/api types
  files/        # opaque, container-mounted files; validated only as YAML if .yml/.yaml
  picolet.yml
```

`picolet.yml` is optional service metadata ([Hooks](#hooks)), read only at the bundle
root; it deploys no resource by itself. A bundle without a deployable file is an
error, as is a missing `services/<name>/` or one that is not a directory. Keep a
`Containerfile` or documentation under the bundle's `files/`, or outside the bundle.

Bundled manifests and files keep their real repo path for template rendering, but Picolet
strips the `services/<name>/` prefix when deriving the deployed destination. For
example, `services/web/manifests/app/deployment.yml.tmpl` deploys to
`/var/lib/picolet/manifests/app/deployment.yml`; `services/web/files/rules.yml.tmpl`
deploys to `/var/lib/picolet/files/rules.yml`.

Collision detection happens during `resolve` / `validate`. Picolet rejects:

- quadlet files that would overwrite another file in the shared quadlet directory
- manifest or file entries that normalize to the same deployed path within their category
- secrets that normalize to the same `secret:<name>` destination, such as
  `foo.yml` and `foo.yaml`

### Raw systemd units (timers, sockets, services)

Files with a systemd unit extension (`.service`, `.timer`, `.socket`, `.target`,
`.path`) outside a first-level `manifests/`, `files/` or `secrets/` directory
are hand-written systemd units deployed verbatim (templated if they end in
`.tmpl`). The unit name is the filename with any `.tmpl` stripped, so
`systemd/maintenance.timer` becomes the unit `maintenance.timer`. Picolet prepends a
`# Managed by picolet` marker, then on apply:

- runs `daemon-reload`,
- **enables** any unit that declares an `[Install]` section (linking its
  `*.target.wants` symlink so it survives reboots),
- **starts** passive activator units (`.timer`/`.socket`/`.target`/`.path`) — and
  **restarts** them on a content change so a new schedule takes effect,
- **enables + restarts** a raw `.service` that declares `[Install]` **unless it is
  `Type=oneshot`** (a long-running daemon managed directly),
- leaves a **`Type=oneshot` job that systemd activates** — fired by a `.timer`, or a
  raw unit with **no `[Install]`** (systemd reports it `static`) — reported but never
  started or restarted, on apply **or** by the health loop; a raw one-shot *with*
  `[Install]` is enabled and started **once** on first deploy, then never re-run on
  edit.

Because Picolet never re-invokes a one-shot systemd owns, **retries belong in the
unit**: `Restart=on-failure` plus `RestartSec=` (both `[Service]`), bounded by
`StartLimitIntervalSec=`/`StartLimitBurst=` (both `[Unit]`). Not `OnFailure=`, which
activates a *handler* unit rather than retrying. `Restart=always` and
`Restart=on-success` are **rejected for `Type=oneshot`** — the unit fails to load;
the legal values are `no`, `on-failure`, `on-abnormal`, `on-abort`, `on-watchdog`.

The Quadlet form of a scheduled job is a `.container` with `[Service] Type=oneshot`
plus a raw `.timer` targeting the generated `<name>.service`, **without**
`RemainAfterExit=yes` (`podman-systemd.unit.5` recommends it for one-shot containers
but warns it breaks subsequent timer activations). For `Type=oneshot` the start
timeout is **disabled by default** (a systemd default, not a Quadlet quirk), so set a
finite `TimeoutStartSec=` if a hung image pull must be bounded — non-one-shot Quadlet
units keep the 90s `DefaultTimeoutStartSec` that image pulls routinely exceed.

On removal Picolet stops and **disables** the unit (removing the enable symlink) and
deletes the file. Deployed units appear in `/metrics` and the dashboard and are
health-monitored; passive units (`.timer`/`.socket`/`.target`/`.path`) and one-shots
systemd activates report their state but are never auto-restarted by the health loop.
Enable/disable/start/restart outcomes are exported as
`picolet_systemd_unit_operations_total{operation,result}` — a restart skipped because
the unit is a timer-triggered one-shot is recorded with `result="skipped"`.

Two known one-shot edge cases are tracked but not yet handled:

- A transient D-Bus failure during the pre-restart status check of a **non-timer**
  one-shot fails closed (the restart is skipped and not retried), so a needed
  restart-on-change can be silently lost ([#122](https://github.com/schjan/picolet/issues/122)).
- Editing a running **daemon** in place into a `Type=oneshot` unit leaves the old
  daemon process running, since only the new content is inspected and `daemon-reload`
  does not stop it ([#123](https://github.com/schjan/picolet/issues/123)).

#### Example: a `podman image prune -a` maintenance timer

`systemd/maintenance.timer`:

```ini
[Unit]
Description=Daily podman image prune

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
```

`systemd/maintenance.service` (oneshot, no `[Install]` — triggered by the timer):

```ini
[Unit]
Description=Prune unused podman images

[Service]
Type=oneshot
ExecStart=/usr/bin/podman image prune -af
```

`assignments.yml`:

```yaml
base:
  paths:
    - systemd/maintenance.timer
    - systemd/maintenance.service
```

> **Prune ↔ reconcile race.** A standalone prune timer can fire while Picolet is
> pulling new images mid-reconcile, deleting layers it is about to use. Mitigate with
> `RandomizedDelaySec=` and/or an `After=` ordering against picolet's unit. For
> guaranteed serialization, prefer the in-process prune (run by the agent between
> reconcile ticks) over a standalone timer.

### Hooks

Service bundles can declare actions to run after assigned Podman secrets,
manifests, or opaque files change. Put them in `services/<name>/picolet.yml` or
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

Hooks run after secret, manifest, or file creates/updates and before normal unit
restarts. If multiple changed secrets, manifests, or files match one hook,
Picolet runs that hook once. File and manifest deletes do not fire hooks. If the
unit is already scheduled for restart because its Quadlet changed, Picolet skips
reload hooks for that unit.

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

Picolet applies the same `retry_pending` treatment to **failed unit restarts**
(independent of hooks). When a managed unit's post-apply restart fails, picolet
records it in `state.json` under `pending_units` — with the originating git SHA,
a consecutive-attempt count, and timestamps — reports the reconciliation as
`retry_pending` rather than a clean success (the SHA is recorded but
`picolet_last_successful_reconciliation_timestamp` is not advanced), and keeps
retrying the unit on every tick via health enforcement, subject to a 5-minute
per-unit cooldown. The `pending_units` record and its cooldown survive an agent
restart, and each pending unit is exposed as the `picolet_unit_restart_pending`
gauge. A unit clears from `pending_units` once it is observed healthy or is
removed from the fleet.

For services whose config is mounted through Podman secrets, verify that the
running container sees replaced secret content on your target Podman version.
If not, use `action: restart`.

### Templates

Files ending in `.tmpl` are rendered with Go `text/template` (`missingkey=error`) plus Sprig's hermetic text helpers. Static files are deployed as-is.

The template data root exposes `.Images`, `.Ports`, `.Fleet` (all hosts + full config), and `.Host`. Every entry of `.Fleet.Hosts` carries the same fields as `.Host` except `Services` and `SystemdUnits`:

| Field | Contents |
|-------|----------|
| `.Host.Hostname` | The host's name |
| `.Host.ExternalHostname` | The host's external hostname |
| `.Host.Role` | The host's `role` |
| `.Host.Features` | The host's enabled features |
| `.Host.Machine` | The Machine the host runs on (`machine:`, defaults to the hostname) |
| `.Host.User` | The Linux user the host's Agent runs as; empty for the rootful host |
| `.Host.Rootful` | `true` for the rootful host (no `user:`) |
| `.Host.ListenPort` | The port the host's Agent listens on (`listen_port:` or its `fleet.yml` default) |
| `.Host.Services` | Resolved service-bundle names for this host (sorted, deduplicated) |
| `.Host.SystemdUnits` | Systemd unit names picolet manages on this host — quadlet-derived (`.container`/`.kube`/`.network`/`.volume`/`.pod`/`.image`/`.build`) plus raw systemd files, sorted and deduplicated. See [Two-pass rendering](#two-pass-rendering) |

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
| `manifestPath(relPath)` | Return the absolute deployed path for a manifest file (handles rootless/rootful, and `host_data_dir` for containerized picolet). `relPath` is relative to the service's `manifests/` dir |
| `filePath(relPath)` | Return the absolute deployed path for a file (handles rootless/rootful, and `host_data_dir` for containerized picolet). `relPath` is relative to the service's `files/` dir |
| `has(item, slice)` | Sprig: check if a value is present in a list |
| `siblings` | The other hosts on this host's Machine (same entries as `.Fleet.Hosts`), sorted by hostname |

Use this when runtime expects one file but you want many repo fragments. Example:

```yaml
groups:{{ concatFiles "rules/vmalert/*.yml" | nindent 2 }}
```

Keep fragments unindented (`- name: ...`) and let the template handle indentation with `nindent`.

`concatFiles` is intentionally raw-only (it does not auto-render matched `.tmpl` files). If you need rendered fragments, use `glob`, iterate, and call `renderTemplate` explicitly in your template.

### Two-pass rendering

Some template data only becomes knowable after `.tmpl` files render, so picolet renders in two passes:

- **First pass** — picolet renders templates with placeholder data. Executing every `.tmpl` file collects secret references (`op://`, `pass://`) so each provider can batch-resolve them; quadlet renders are additionally parsed with Podman's unit-name resolver to derive `.Host.SystemdUnits`. First-pass render errors are non-fatal.
- **Final pass** — every template renders again with fully populated data (resolved secrets and `.Host.SystemdUnits`); this pass is the source of truth for diagnostics.

A template consuming `.Host.SystemdUnits` sees it empty during the first pass, so iterate it with `range` rather than indexing — and a template whose own filename or `ServiceName=` depends on `.Host.SystemdUnits` cannot be resolved. See the `preparedData` doc comment in `pkg/resolver/resolver.go` for the full rationale.

### Validation

All files are validated before deployment: quadlet files via Podman's own `quadlet.Convert*()`, K8s manifests via strict unmarshalling into `k8s.io/api` types, opaque files as YAML syntax only when their rendered source extension is `.yml` or `.yaml`, systemd units structurally, and templates at render time (`missingkey=error`).

Secrets always require non-empty content. Repo-backed YAML secrets (`.yml` / `.yaml`, including `.tmpl`) are also syntax-validated after template rendering. External placeholder secrets in repo-only validation mode are skipped for YAML syntax checks.

Run `./picolet validate` in CI to catch errors before pushing.

## Secret Providers

Picolet integrates with two secret managers so cleartext credentials never live in the fleet repo. Both providers can be configured at the same time; references are routed by URI scheme.

| Provider | Scheme | Underlying tool | Auth model |
|----------|--------|-----------------|------------|
| 1Password | `op://vault/item/field` | Official Go SDK | Service-account token |
| Proton Pass | `pass://share/item/field` | `pass-cli` (bundled in the container image) | Personal Access Token |

Refs can appear in two places:

- **Direct Podman secrets** in `assignments.yml` under `secrets:` — Picolet resolves the value and creates a Podman secret named after the URI components (e.g. `vault_item_field` or `share_item_field`).
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
