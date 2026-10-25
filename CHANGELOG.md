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
- `picolet bootstrap create` and the shell scripts
  `deploy/bootstrap/bootstrap.sh` / `bootstrap-rootless.sh` are removed;
  `picolet bootstrap machine` replaces them
  ([#156](https://github.com/schjan/picolet/issues/156)).
- The Agent checks every credential file its config names at startup, not only
  the ones it opened before: `webhook_secret_path`, `protonpass.pat_path`,
  `mqtt.password_path`, `git_token_path`, `github_private_key_path`,
  `onepassword.token_path` and `sops.age_identity_path` must exist and be
  readable, or the Agent exits (`credential <setting> (<path>) is not
  readable`). Run the pre-upgrade check in
  [MIGRATION.md](MIGRATION.md#pre-upgrade-check-agent-credential-files) before
  bumping `images.picolet`
  ([#252](https://github.com/schjan/picolet/issues/252)).
- An Agent config in which two credential settings name the same file fails to
  load (`… name the same credential file`), and `readSecretFile` and file
  `secrets:` refuse every Agent credential file: *Agent credentials cannot be
  deployed*, in the Agent and in `picolet bootstrap` (the credentials of the
  Agent config it renders, below `--secrets-dir`)
  ([#251](https://github.com/schjan/picolet/issues/251)).
- SOPS files (`<name>.sops.<ext>`) and SOPS's `.sops.yaml`/`.sops.yml` are
  skipped by `paths:` directory and Service Bundle walks, and an explicit
  `paths:` or plain `secrets:` entry naming a SOPS file is an error; a SOPS file
  is reachable only through a `sops://` ref
  ([#255](https://github.com/schjan/picolet/issues/255)).
- `picolet validate` renders each Host's assigned Agent bundle strictly, as
  bootstrap does: a provider template-function call (`readOpSecret`,
  `readProtonPassSecret`, `readSopsSecret`, `readSopsData`) or a direct provider
  `secrets:` ref in the Agent bundle fails CI; YAML-value refs such as
  `onepassword.git_token_ref` pass
  ([#253](https://github.com/schjan/picolet/issues/253)).

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
- `picolet bootstrap teardown --machine <machine> --repo-dir <checkout>` tears
  down every Host of the Machine: each Host's `bootstrap teardown` runs in the
  per-Host bootstrap's container, as the Host's user or as root. Users,
  subuid/subgid ranges and lingering are never touched
  ([#156](https://github.com/schjan/picolet/issues/156)).
- SOPS-encrypted values in the Fleet
  ([ADR 0003](docs/adr/0003-no-plaintext-secret-in-git.md),
  [#248](https://github.com/schjan/picolet/issues/248)):
  `sops://<fleet-path>[#<json-pointer>]` Secret References, the template
  functions `readSopsSecret` (one value) and `readSopsData` (a subtree), and
  direct `sops://` entries in `secrets:`. The Agent decrypts with the age
  identities named by the new Agent config block `sops: {age_identity_path: …}`;
  `validate`, `resolve` and `bootstrap` check refs without a key. A Quadlet, Raw
  Unit or Hook definition that renders a SOPS value fails.
  `picolet dry-run --config` decrypts like the Agent; decrypted values are
  scrubbed from errors, logs, events, MQTT and deployment statuses
  ([#256](https://github.com/schjan/picolet/issues/256),
  [#257](https://github.com/schjan/picolet/issues/257),
  [#258](https://github.com/schjan/picolet/issues/258),
  [#259](https://github.com/schjan/picolet/issues/259),
  [#260](https://github.com/schjan/picolet/issues/260),
  [#266](https://github.com/schjan/picolet/issues/266),
  [#267](https://github.com/schjan/picolet/issues/267)).
- `host.yml` `age_recipients:` (X25519 or hybrid post-quantum age keys) and
  `.Host.AgeRecipients`; `validate` fails a `sops://` ref of a Host that is not
  a Recipient of the file
  ([#261](https://github.com/schjan/picolet/issues/261)). The reference Agent
  config templates emit the `sops:` block only for a Host with
  `age_recipients:`, and the Agent warns when none of its identities is among
  them ([#268](https://github.com/schjan/picolet/issues/268)).
- `operators.yml`: Operators' age Recipients, read only by `validate`, which
  fails every Recipient of a SOPS file that is neither an Operator's nor a
  consuming Host's ([#262](https://github.com/schjan/picolet/issues/262)).
- New `validate` passes over every SOPS file in the Fleet: plaintext or
  oversized `*.sops.*` files and files whose Recipients drifted from their
  Fleet-root `.sops.yaml` creation rule fail
  ([#263](https://github.com/schjan/picolet/issues/263)).
- `picolet_secrets_managed_count`, `picolet_secret_sync_total` and
  `picolet_secret_last_sync_timestamp` gain `provider="sops"`.
- CI runs `govulncheck` (on every change and weekly) against the built binary's
  dependencies, failing on any finding not excluded with a reason
  ([#254](https://github.com/schjan/picolet/issues/254)).

### Changed

- An Agent restart clears the failed-SHA gate (and records a
  `failed_sha_gate_cleared` dashboard event), so a fix outside git, such as a
  credential file, takes effect without a new commit
  ([#249](https://github.com/schjan/picolet/issues/249)).
- The dashboard no longer shows content hashes of secrets, nor of Managed
  Files whose render used a SOPS value
  ([#250](https://github.com/schjan/picolet/issues/250)).
- The self-restart guard refuses a Fleet-rendered Agent config the Agent could
  not start with (strict parse, credential overlap, startup credential checks):
  the reconciliation fails and the Agent keeps running on its current config
  ([#252](https://github.com/schjan/picolet/issues/252)).
- Agents warn about an unknown `host.yml` key instead of refusing the Fleet;
  `picolet validate` still fails on it.
- `picolet_unit_state_info` is emitted for inactive units too.
