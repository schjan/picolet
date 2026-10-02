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
  `picolet_host_info{pi_type}` → `{role}`, `fleet.yml` `prometheus:` removed
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
- `host.yml` `machine:`, `user:`, `listen_port:`; `.Host.Machine`, `.Host.User`,
  `.Host.ListenPort` and the `siblings` template function
  ([#157](https://github.com/schjan/picolet/issues/157)).
- `picolet_host_info` labels `machine` and `user`; machine and user in the
  dashboard and `picolet resolve` output
  ([#158](https://github.com/schjan/picolet/issues/158)).
- `picolet_files_managed_total{category}` values `pod`, `image`, `build`.
- `picolet bootstrap machine --plan` (read-only)
  ([#151](https://github.com/schjan/picolet/issues/151)).

### Changed

- Agents warn about an unknown `host.yml` key instead of refusing the Fleet;
  `picolet validate` still fails on it.
- `picolet_unit_state_info` is emitted for inactive units too.
