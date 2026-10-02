# Migrating a Fleet to v0.2.0

v0.2.0 breaks the Fleet schema. An Agent of this release refuses to load a Fleet
that still uses a removed key, and an older Agent refuses to load a Fleet that
uses a new one. Migrate in the order under [Rollout order](#rollout-order): a
wrong order leaves Hosts that can no longer update themselves.

Part of this migration already shipped in v0.1.34: the `pi_type` → `role`
rename, the removal of `fleet.yml` `prometheus:` and the loopback listener (the
rows with Since = v0.1.34 below). A Fleet that runs v0.1.34 has them behind it;
a Fleet on v0.1.33 or older needs all of them.

## What breaks

| Change | Since | File | Error from `picolet validate` |
|--------|-------|------|-------------------------------|
| `pi_type:` → `role:` | v0.1.34 | `hosts/<name>/host.yml` | `host.yml: 'pi_type:' was renamed to 'role:'` |
| `pi_types:` → `roles:` | v0.1.34 | `assignments.yml` | `assignments.yml: 'pi_types:' was renamed to 'roles:'` |
| `prometheus:` removed | v0.1.34 | `fleet.yml` | `fleet.yml: 'prometheus:' was removed from the schema; delete it` |
| Agent listener binds loopback by default | v0.1.34 | Agent config (`/etc/picolet/config.yml`) | none: the Agent starts and is unreachable from other machines |
| `.Host.PiType` → `.Host.Role`, `.Fleet.Hosts[].PiType` → `.Role`, `.Fleet.Config.Prometheus` removed | v0.1.34 | templates | template execution error (`can't evaluate field PiType` / `Prometheus`) |
| `picolet_host_info{pi_type}` → `picolet_host_info{role}` | v0.1.34 | alert rules, dashboards | none: queries on `pi_type` match nothing |
| Typed lists removed: `networks:`, `volumes:`, `containers:`, `kube:`, `systemd:`, `manifests:`, `files:` (and `pods:`, `images:`, `builds:`, which no release shipped) | v0.2.0 | `assignments.yml` | `assignments.yml: <group>: '<key>:' was removed; list these files under 'paths:', which derives the category from the file name` (one line per occurrence) |
| A file with an unknown extension outside `files/`/`manifests/`/`secrets/` is rejected | v0.2.0 | `paths:` entries, Service Bundles | names the file |
| systemd units other than `.service`, `.timer`, `.socket`, `.target`, `.path` (e.g. `.mount`, `.slice`, `.automount`, `.swap`) are rejected; v0.1.34 deployed any file under `systemd:` or a bundle's `systemd/` | v0.2.0 | `assignments.yml`, Service Bundles | `<path>: unknown extension ".mount"; move it under files/ or remove it from the listed directory` |
| A path containing two of `files/`, `manifests/`, `secrets/` (e.g. `files/app/secrets/x.conf`) is rejected | v0.2.0 | `paths:` entries, Service Bundles | `path has both "files/" and "secrets/"; …` |
| Strict Service Bundle rules removed | v0.2.0 | `services/<name>/` | none: typed-subdirectory layouts stay valid, except for the two rows above |
| `ports.picolet_metrics` / `ports.picolet_system_metrics` required (unless every Host sets `listen_port:`) | v0.2.0 | `fleet.yml` | the Fleet fails to load |
| A rootless Host needs `user:`; a Host whose `hostname` is not a hostname label needs `machine:` | v0.2.0 | `hosts/<name>/host.yml` | without `user:`, no error, the Host counts as rootful; without `machine:`, the Fleet fails to load |
| Two Hosts on one Machine with the same `user:` (two rootful Hosts included) or the same listen port are rejected; `machine:` defaults to the `hostname`, compared case-insensitively | v0.2.0 | `hosts/<name>/host.yml`, `fleet.yml` | the collision names both Hosts; the Fleet fails to load |

`state.json` needs nothing: keep it. Its `managed_files[].category` values are
unchanged rows of the new category table, so an Agent of this release loads the
file written by an older one, and orphan removal, health checks and
`picolet_files_managed_total` keep working.

`picolet bootstrap create` and the shell scripts in `deploy/bootstrap/` still
ship in v0.2.0; see
[Bootstrap](#bootstrap-bootstrap-create-and-the-shell-scripts) for their
planned replacement.

## Rollout order

Every Agent loads the whole Fleet, so one commit is read by old and new Agents
alike. Agents update themselves: an Agent applies the new `images.picolet` tag
from `fleet.yml` to its own Quadlet and restarts onto it. An Agent that cannot
load the Fleet applies nothing, its own image included.

1. **Upgrade the image on all Hosts, schema unchanged.** In one commit, bump
   `images.picolet` in `fleet.yml` to `v0.2.0` and add both
   `ports.picolet_metrics` and `ports.picolet_system_metrics` if missing (plain
   `ports` entries, which older Agents accept; until step 3 adds `user:`, a v0.2.0
   Agent treats every Host as rootful and takes `picolet_system_metrics`). Do not
   touch anything else.
   Every old Agent loads this commit and restarts onto v0.2.0.
2. **Wait until every Host runs v0.2.0**: the Agent logs `agent started` with
   `version=v0.2.0`, and `picolet_build_info{version="v0.2.0"}` is 1. From now
   on, each upgraded Agent whose Fleet still uses a removed key fails to load
   it: reconciliation stops on that Host, no Fleet change is applied, and the
   running containers keep running. A CI job that runs `picolet validate` with
   the image pinned in `fleet.yml` fails on the commit from step 1 for the same
   reason. Step 3 ends both.
3. **Migrate the schema**, in one commit: every key in
   [Key by key](#key-by-key), the Agent config template, and the `host.yml`
   keys that are required for your Hosts (`user:` on rootless Hosts, `machine:`
   where the hostname is not a hostname label, and `machine:`/`user:`/
   `listen_port:` wherever two Hosts would otherwise collide on one Machine).
   Run `picolet validate` of v0.2.0 on it before pushing.
4. **Then add optional `host.yml` keys** (`machine:`, `user:`, `listen_port:`)
   wherever you want them. Never before step 2: an older Agent rejects the
   whole Fleet on a `host.yml` key it does not know. Agents of this release only
   warn about an unknown `host.yml` key, so later releases can add keys without
   this ordering (`picolet validate` still fails on one, so typos break CI).

Doing step 3 before step 2 has finished strands every Host that is still old:
it cannot load the migrated Fleet, so it never applies the new image. Recover
such a Host by hand: set `Image=` in its picolet Quadlet
(`/etc/containers/systemd/picolet/picolet-system.container`, rootless
`~/.config/containers/systemd/picolet/picolet.container`) to the v0.2.0 tag,
then `systemctl daemon-reload && systemctl restart picolet-system`
(`systemctl --user …` and `picolet` for rootless). The upgraded Agent loads the
migrated Fleet and takes its Quadlet back over.

**Fleets that need no stop.** A Fleet that v0.1.34 and v0.2.0 both accept
upgrades through steps 1 and 2 without a stop. The test is direct: run
`picolet validate` of v0.2.0 (the release binary, or
`podman run --rm -v "$PWD:/repo:ro" ghcr.io/schjan/picolet:v0.2.0 --repo-dir /repo validate`)
on the step-1 commit; if it passes, no Host stops. It can pass only when the Fleet
uses `role:`/`roles:` (v0.1.34) and no `prometheus:` key, assigns everything
through `services:` and `secrets:`, keeps its bundles in the old typed
subdirectories (no systemd unit with an extension v0.2.0 rejects, no nested
`files/`/`manifests/`/`secrets/` directory), and needs no new `host.yml` key:
every `hostname` is a hostname label, and no two Hosts collide on a Machine
(Machines default to the hostname and compare case-insensitively, so `node`
and `NODE` are one Machine with two rootful Hosts). Flatten bundles (see
[Service Bundles](#service-bundles-v020)) only after step 2.

## Key by key

### `host.yml`: `pi_type:` → `role:` (v0.1.34)

```yaml
# before
hostname: rpi5-1
pi_type: worker
# after
hostname: rpi5-1
role: worker
```

No alias: `pi_type:` is rejected even when empty or set alongside `role:`.

### `assignments.yml`: `pi_types:` → `roles:` (v0.1.34)

```yaml
# before
pi_types:
  worker:
    services: [picolet-system]
# after
roles:
  worker:
    services: [picolet-system]
```

### `fleet.yml`: `prometheus:` removed (v0.1.34)

Delete the key, and replace every template reference to it
(`.Fleet.Config.Prometheus`, e.g. `{{ .Fleet.Config.Prometheus.scrape_interval }}`)
with a literal value or a `ports:`/`images:` entry: the field is gone, so such a
template fails to render (`can't evaluate field Prometheus`). `fleet.yml` holds
`images:` and `ports:` only. A `ports:` entry named `prometheus` is a port name
and still loads.

### `fleet.yml`: Agent ports (v0.2.0)

Every Host needs an Agent listen port: `listen_port:` in its `host.yml`, else
`ports.picolet_metrics` when the Host has `user:`, else
`ports.picolet_system_metrics`. Templates read it as `.Host.ListenPort`.

```yaml
ports:
  picolet_metrics: 9417
  picolet_system_metrics: 9418
```

### `assignments.yml`: typed lists → `paths:` (v0.2.0)

A group (`base`, each role, each feature) accepts exactly `paths:`, `secrets:`
and `services:`. Move every entry of the ten retired lists — `networks:`,
`volumes:`, `containers:`, `kube:`, `pods:`, `images:`, `builds:`, `systemd:`,
`manifests:` and `files:` — into the group's `paths:`; `secrets:` and
`services:` stay as they are. (`pods:`, `images:` and `builds:` existed only on
development builds between v0.1.34 and v0.2.0.)

```yaml
# before
base:
  networks:
    - quadlets/networks/internal.network
  systemd:
    - systemd/maintenance.timer
    - systemd/maintenance.service
roles:
  controller:
    containers:
      - quadlets/containers/exporter.container
    volumes:
      - quadlets/volumes/data.volume
    kube:
      - quadlets/kube/app-stack.kube.tmpl
    manifests:
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
# after
base:
  paths:
    - quadlets/networks/internal.network
    - systemd/maintenance.timer
    - systemd/maintenance.service
roles:
  controller:
    paths:
      - quadlets/containers/exporter.container
      - quadlets/volumes/data.volume
      - quadlets/kube/app-stack.kube.tmpl
      - manifests/app/deployment.yml.tmpl
    secrets:
      - secrets/app_secret.yml.tmpl
```

The category now comes from the path (README
[`paths:` entries](README.md#paths-entries)): a first segment `manifests/`,
`files/` or `secrets/` selects that category, otherwise the extension decides
(`.tmpl` stripped first). Consequences when moving entries:

- Quadlet units and `.service`, `.timer`, `.socket`, `.target` and `.path`
  units deploy where they did; which directory they sit in no longer matters.
- Any other file under the old `systemd:` list (`.mount`, `.slice`,
  `.automount`, `.swap`, …) is now an error, and v0.2.0 has no way to deploy it:
  remove it from the Fleet and manage it on the Machine outside picolet.
  Removing it from the Fleet is a removal like any other: the Agent stops,
  disables and deletes the deployed unit. To keep it running, install it on
  the Machine by hand after the Reconciliation that removed it.
- A former `manifests:` or `files:` entry must start with `manifests/` or
  `files/`. Move one that does not, and its deployed path moves with it to
  `<data dir>/manifests/…` or `<data dir>/files/…`; update `filePath` /
  `manifestPath` references and `Volume=` mounts that point at the old path.
- A file with an unknown extension or none (`Containerfile`, `README.md`,
  `.gitkeep`) outside `files/`/`manifests/`/`secrets/` is an error; move it
  under `files/` or out of the listed directory.
- `paths:` also takes directories, expanded recursively: `files/` deploys every
  File below it.

### Service Bundles (v0.2.0)

A bundle `services/<name>/` is now a plain directory, expanded like a directory
under `paths:`. The strict bundle rules (fixed subdirectories, nothing loose at
the bundle root, no nesting below unit subdirectories) are gone; `manifests/`,
`files/` and `secrets/` keep their meaning. Old layouts (`containers/`,
`volumes/`, `networks/`, `kube/`, `systemd/`) stay valid, with two exceptions:
a unit in `systemd/` with an extension other than `.service`, `.timer`,
`.socket`, `.target`, `.path`, and a `files/`, `manifests/` or `secrets/`
directory nested below another one (`files/app/secrets/`), which now selects two
categories at once; rename that directory. Flattening is optional, and only
after every Host runs v0.2.0: v0.1.34 rejects a unit at the bundle root as
`unknown entry`.

```text
# before                                        # after (optional)
services/picolet-system/containers/             services/picolet-system/
  picolet-system.container.tmpl                   picolet-system.container.tmpl
services/picolet-system/secrets/                services/picolet-system/secrets/
  picolet_system_config.yml.tmpl                  picolet_system_config.yml.tmpl
```

### Templates (v0.1.34, v0.2.0)

| Before | After |
|--------|-------|
| `.Host.PiType` | `.Host.Role` |
| `.Fleet.Hosts[].PiType` | `.Fleet.Hosts[].Role` |
| `.Fleet.Config.Prometheus` | removed; use literals or `.Fleet.Config.Ports` / `.Ports` |

New in v0.2.0, readable only by v0.2.0 Agents (so use them from step 3 on):
`.Host.Machine`, `.Host.User`, `.Host.ListenPort` (also on every
`.Fleet.Hosts` entry) and the `siblings` function.

### Agent config: `listen_addr` and the loopback default (v0.1.34)

Before v0.1.34 the Agent served `/metrics`, `/health`, `/webhook` and the
dashboard on every interface (`:9417`, or `:<metrics_port>`). Since v0.1.34 it
binds `127.0.0.1:<metrics_port>` (default `127.0.0.1:9417`), because none of
these endpoints is authenticated. `metrics_port:` still sets the port.

- A Prometheus on the same Machine keeps working: scrape `127.0.0.1:<port>`.
- A Prometheus on another machine, or a webhook sender reaching the Agent
  directly, stops reaching it. Either go through a reverse proxy or mesh
  network, or bind deliberately: `listen_addr: "0.0.0.0:9417"` (every
  interface) or `listen_addr: "192.168.1.20:9417"` (one interface).
  `metrics_port:` and a `listen_addr:` with a port must name the same port, or
  the Agent refuses to start.
- **`Network=host` caveat.** A containerized Agent reaches the Machine's
  loopback only with `Network=host`, as both reference Quadlets have. An Agent
  container with its own network namespace binds its own loopback, unreachable
  from the Machine, `picolet trigger` and Prometheus included: give it
  `Network=host`, or bind `listen_addr: "0.0.0.0:<port>"` and publish the port.
  The Agent logs a warning when it detects a loopback bind in its own network
  namespace.

The reference Agent config templates now take the address from the Fleet;
switch to this form in step 3, since `.Host.ListenPort` needs a v0.2.0 Agent:

```yaml
# before (services/picolet-system/secrets/picolet_system_config.yml.tmpl)
metrics_port: {{ index .Ports "picolet_system_metrics" }}
# after
listen_addr: 127.0.0.1:{{ .Host.ListenPort }}
```

### Metrics

| Metric | Change |
|--------|--------|
| `picolet_host_info` | label `pi_type` renamed `role` (v0.1.34); labels `machine` and `user` added (`user="root"` for the rootful Host) (v0.2.0) |
| `picolet_files_managed_total{category}` | new values `pod`, `image`, `build` (v0.2.0) |
| `picolet_unit_state_info` | now also emitted for inactive units (v0.2.0); `picolet_unit_active` is unchanged |

Replace `pi_type` with `role` in alert rules, recording rules and dashboards.

### Bootstrap: `bootstrap create` and the shell scripts

The series replaces `picolet bootstrap create` and both shell scripts
(`deploy/bootstrap/bootstrap.sh`, `deploy/bootstrap/bootstrap-rootless.sh`)
with one idempotent root command, `picolet bootstrap machine`, that bootstraps
every Host of a Machine from the Fleet (`machine:`/`user:`/`listen_port:` in
`host.yml`).

**Status in v0.2.0:** `bootstrap machine` has only its read side,
`picolet bootstrap machine --plan`, which prints the steps it would take and
changes nothing. `bootstrap create` and both scripts still ship and remain the
way to bootstrap a new Host; their removal lands with
[#156](https://github.com/schjan/picolet/issues/156). Already bootstrapped Hosts
are unaffected either way: bootstrap runs once, and the Agent manages itself
from the Fleet afterwards. Start describing your Machines in `host.yml`
(step 4) so that `bootstrap machine` can take over once the scripts are gone.

## Worked example: `deploy/fleet-repo/`

`deploy/fleet-repo/` at v0.1.33 was one Host, `rpi5-1`, with the role `worker`
and the bundle `picolet-system`. The same Fleet, migrated through the steps
above (step 3 and 4 shown together):

`fleet.yml`: unchanged except the image tag (step 1). Both Agent ports were
already there, and it had no `prometheus:` key.

```diff
 images:
-  picolet: "ghcr.io/schjan/picolet:v0.1.0"
+  picolet: "ghcr.io/schjan/picolet:v0.2.0"
```

`assignments.yml`: rename only; the groups used `services:` alone, which needs
no `paths:` move.

```diff
 base: {}

-pi_types:
+roles:
   # Production: rootful Podman, system-level systemd
   worker:
     services:
       - picolet-system
```

`hosts/rpi5-1/host.yml`: a rootful Host whose hostname is a hostname
label, so `machine:` and `user:` are optional; its Agent listens on
`ports.picolet_system_metrics`.

```diff
 hostname: rpi5-1
-pi_type: worker
+role: worker
 features: []
```

`services/picolet-system/`: `containers/picolet-system.container.tmpl` may stay
or move to the bundle root (after step 2). The Agent config template switches to
the Fleet-owned address:

```diff
 hostname: "{{ .Host.Hostname }}"
 repo_url: "https://github.com/example/fleet.git"
 git_token_path: "/etc/picolet/secrets/git_token"
-metrics_port: {{ index .Ports "picolet_system_metrics" }}
+listen_addr: 127.0.0.1:{{ .Host.ListenPort }}
```

The rootless `test` role and `services/picolet/` follow the same pattern; a
rootless Host additionally needs `user:` in its `host.yml`. Today's
`deploy/fleet-repo/` builds on this result with a Machine of three Hosts and
more bundles; those are additions, not migrations (README
[Fleet conventions](README.md#fleet-conventions)).
