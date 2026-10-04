# Changelog

## v0.2.0 (unreleased)

**Breaking minor release.** The Fleet schema changes incompatibly: an Agent of
this release refuses a Fleet that still uses a removed key, and older Agents
refuse a migrated Fleet. Read [MIGRATION.md](MIGRATION.md) before bumping
`images.picolet`, and follow its rollout order: upgrade the image on all Hosts
first, then migrate the schema and add new `host.yml` keys. `state.json` is
kept as is.

### Breaking

- `assignments.yml`: the typed lists `networks:`, `volumes:`, `containers:`,
  `kube:`, `pods:`, `images:`, `builds:`, `systemd:`, `manifests:` and `files:`
  are removed. A group takes `paths:` (files or directories; the category comes
  from the path), `secrets:` and `services:`
  ([#146](https://github.com/schjan/picolet/issues/146),
  [#148](https://github.com/schjan/picolet/issues/148)).
- A file with an unknown extension or none outside a first-level `files/`,
  `manifests/` or `secrets/` directory is a validation error. This includes
  systemd units other than `.service`, `.timer`, `.socket`, `.target`, `.path`
  (e.g. `.mount`), which v0.1.34 deployed from `systemd:`.
- A path containing two of `files/`, `manifests/`, `secrets/` is a validation
  error.
- Service Bundles are plain directories; the strict bundle rules are gone. Old
  typed-subdirectory layouts stay valid
  ([#147](https://github.com/schjan/picolet/issues/147)).
- `fleet.yml` `ports.picolet_metrics` / `ports.picolet_system_metrics` are
  required unless every Host sets `listen_port:`; a rootless Host needs
  `user:` in `host.yml`, and a Host whose `hostname` is not a hostname label
  needs `machine:` ([#157](https://github.com/schjan/picolet/issues/157)).
- Shipped in v0.1.34 and part of the same migration: `pi_type:` → `role:`,
  `pi_types:` → `roles:`, `.Host.PiType` → `.Host.Role`,
  `picolet_host_info{pi_type}` → `{role}`, `fleet.yml` `prometheus:` and the
  `.Fleet.Config.Prometheus` template field removed
  ([#168](https://github.com/schjan/picolet/pull/168)); the Agent listener
  binds loopback by default, `listen_addr` to expose it
  ([#171](https://github.com/schjan/picolet/pull/171)).

### Added

- `.pod`, `.build` and `.image` Quadlets
  ([#183](https://github.com/schjan/picolet/pull/183),
  [#145](https://github.com/schjan/picolet/issues/145)); a changed `.build` or
  build input rebuilds and restarts its consumers
  ([#127](https://github.com/schjan/picolet/issues/127)); `validate` checks that
  a `.build`'s Containerfile and context are delivered
  ([#128](https://github.com/schjan/picolet/issues/128)).
- Hook `unit:` resolves `.pod`, `.build` and `.image` to their generated
  services, and `.Host.SystemdUnits` lists them
  ([#138](https://github.com/schjan/picolet/issues/138)).
- `host.yml` `machine:`, `user:`, `listen_port:`; `.Host.Machine`, `.Host.User`,
  `.Host.ListenPort` and the `siblings` template function
  ([#157](https://github.com/schjan/picolet/issues/157)).
- `picolet_host_info` labels `machine` and `user`; machine and user in the
  dashboard and `picolet resolve` output
  ([#158](https://github.com/schjan/picolet/issues/158)).
- `picolet_files_managed_total{category}` values `pod`, `image`, `build`.
- `picolet bootstrap machine --plan` (read-only)
  ([#151](https://github.com/schjan/picolet/issues/151)).
- `picolet bootstrap machine --secrets-dir <dir>` places `<dir>/<hostname>/`
  into each Host's secrets directory (`0600`, the Host's user), writing only
  new or changed files and listing their Hosts as needing an Agent restart
  ([#153](https://github.com/schjan/picolet/issues/153)).
- `picolet bootstrap machine --onepassword-token-file` / `--protonpass-pat-file`
  places the Machine's provider token (`op-service-account-token` / `pp-pat`)
  for every Host without a `bootstrap:` block; `host.yml` `bootstrap:` (or
  `fleet.yml` `bootstrap.files` / `bootstrap.roles.<role>`, the most specific
  winning whole) lists Secret References bootstrap resolves in one batch and
  places as files for a Host that runs without a provider. Unresolved
  references warn; Proton Pass needs `pass-cli` and keeps no session
  ([#154](https://github.com/schjan/picolet/issues/154)). Upgrade Agents before
  adding `bootstrap:` to `fleet.yml`.
- `picolet bootstrap machine` starts each Host's Agent: the per-Host bootstrap
  runs in a container of the Host's own Podman (as its user via `runuser`, or
  as root) with the Agent quadlet's bind mounts, so it seeds `state.json` with
  the Agent's container paths; an Agent whose credential files the run wrote is
  restarted, every Agent's health is waited for, and a per-Host summary
  (user, probed port, health, restart) ends the run. `picolet bootstrap`
  gains `--skip-health-wait`, which `bootstrap machine` passes so an Agent
  needing a restart for new credentials is restarted before any health wait
  ([#155](https://github.com/schjan/picolet/issues/155)).

### Changed

- Agents warn about an unknown `host.yml` key instead of refusing the Fleet;
  `picolet validate` still fails on it.
- `picolet_unit_state_info` is emitted for inactive units too.
