# What does SOPS age decryption cost picolet's arm64 binary and image?

Research for schjan/picolet#239. All numbers measured on 2026-10-24 against `getsops/sops/v3` **v3.13.3** (latest release, tagged 2026-07-23 [S1]) with Go 1.27.0.

## Answer

- **The official `decrypt.Data` path costs +23.4 MiB (+47.7 %)** on the stripped linux/arm64 binary: 49.1 MiB goes to 72.6 MiB. Compressed with gzip, which is roughly what the image layer adds, it costs +6.5 MiB (+38.9 %). The module graph grows from 490 to 663 modules (+173), the modules linked into the binary from 156 to 248 (+92), and the packages compiled from 1052 to 1649. Of the binary growth, 0.25 MiB comes from shared modules that `go get` upgrades (gRPC, OpenTelemetry). The rest is SOPS and the KMS/storage SDKs it links in. (Tables M1 and M2.)
- **SOPS has no narrower import path and no build tags.** The root `sops` package imports `keyservice`, which imports every key source (AWS KMS, GCP KMS, Azure Key Vault, Vault, Huawei KMS, PGP) [S2][S3]. Every store imports `config`, which also imports `publish` (S3, GCS, Vault) [S4][S5]. Skipping `decrypt` and calling `stores/yaml` + `aes` directly saves only 0.1 MiB. Even a non-working "floor" build without any store or `config` still adds +10.6 MiB (+21.6 %). Upstream issue getsops/sops#2157 asks for an age-only module and has no maintainer response yet [S6]. The related plugin proposal #2016 is open with no direction chosen [S7].
- **I found no maintained third-party Go library that reads SOPS files on its own.** Every Go package I found wraps or forks `getsops/sops/v3`, so it pulls the same dependencies [S8–S11]. The only independent implementations I found are in other languages: TypeScript `sops-age` [S12] and Perl `File::SOPS` [S13].
- **A minimal age-only reimplementation is small and cheap.** A 364-line prototype (filippo.io/age + `go.yaml.in/yaml/v4`, which picolet already uses, + stdlib AES-GCM/SHA-512) adds **+0.19 MiB (+0.38 %)**, +4 modules (+2 linked into the binary) and +11 packages. On five files encrypted by the real `sops` 3.13.3 CLI, its YAML output is byte-identical to `sops -d` (plain, two recipients, `unencrypted_suffix`, `encrypted_regex`, `mac_only_encrypted`). JSON decrypts and passes MAC verification but is re-emitted as flow YAML. Tampered ciphertext and tampered unencrypted values are both rejected, the latter by a MAC mismatch that matches the one `sops` reports.
- **Recommendation:** do not import `getsops/sops/v3` while #2157 is unresolved. A +47.7 % binary for one key type is out of proportion. Instead, write a small in-tree age-only decryptor limited to YAML (and possibly JSON/binary). Pin it to the SOPS format with golden fixtures produced by the real `sops` CLI and a CI job that re-encrypts them with each new SOPS release. Reject anything it does not cover (Shamir with threshold > 1, non-age-only key groups, `time` values, dotenv/INI) with a clear error. Revisit if upstream ships build tags or a slim module.

## Method

- **Isolation.** I copied the picolet checkout to a scratch directory and made every variant from that copy. The repository itself was not modified.
- **Build command.** All variants used the command from `Containerfile` / `Taskfile.yml` `build-arm64` [S14]:
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" -ldflags "-X …Version=dev -X …GitSHA=unknown -s -w" ./cmd/picolet`.
  `-trimpath` comes from the Containerfile. The Taskfile omits it, which does not change the deltas.
- **Wiring.** Each variant adds one file to `cmd/picolet` with an `init()` that runs the decrypt call only when an environment variable is set. The code is therefore reachable and the linker cannot drop it. Then `go get …@v3.13.3` and `go mod tidy`.
- **Variants:**
  - `base`: unmodified picolet.
  - `upgr`: control build. It uses `full`'s `go.mod`/`go.sum` without the SOPS file, so it measures only the shared-module upgrades that `go get` pulls in.
  - `full`: `decrypt.Data(b, format)`.
  - `narrow`: `stores/yaml.NewStore` + `Metadata.GetDataKey` + `aes.NewCipher` + `Tree.Decrypt` + MAC check, i.e. `decrypt.DataWithFormat` inlined without `cmd/sops/common`.
  - `core`: root `sops` + `aes` + `age` + `stores` only, with no store and no `config`. It is not a working decryptor. It shows the floor of any approach built on SOPS packages.
  - `min`: the prototype under `internal/sopsage`.
- **Counts.**
  - Binary size: `stat`.
  - Compressed size: `gzip -6`, which stands in for the registry layer. The image's other layers (debian-slim, git, pass-cli) do not change, so the image grows by the size of the `/picolet` layer.
  - Modules: `go list -m all | wc -l` for the graph, and the `dep` lines of `go version -m` for modules actually linked into the binary.
  - Packages: `go list -deps ./cmd/picolet` with the same tags, GOOS and GOARCH.
- **Per-dependency attribution.** Unstripped builds of `full` and `upgr`, then `go tool nm -size`, with symbol sizes summed by module path.
- **Prototype check.**
  - Fixtures: I generated two keys with `age-keygen` v1.3.1 and encrypted the fixtures with a `sops` 3.13.3 CLI built from the module cache.
  - Comparison: the prototype's output was diffed against `sops -d --output-type yaml`.
  - Tamper tests: a modified ciphertext, and a modified unencrypted value in an `encrypted_regex` file.

## Details

### M1: size per variant (linux/arm64, stripped)

| Variant | Binary (bytes) | Δ binary | gzip (bytes) | Δ gzip | `go list -m all` | modules in binary | packages |
|---|---:|---:|---:|---:|---:|---:|---:|
| base | 51,511,456 (49.1 MiB) | — | 17,404,542 | — | 490 | 156 | 1052 |
| upgr (shared-module bumps only) | 51,773,600 | +0.25 MiB (+0.51 %) | 17,469,943 | +0.38 % | 663¹ | 156 | — |
| **full** (`decrypt.Data`) | 76,087,456 (72.6 MiB) | **+23.4 MiB (+47.71 %)** | 24,179,144 | **+6.46 MiB (+38.92 %)** | 663 | 248 | 1649 |
| narrow (stores/yaml + aes) | 75,956,384 | +23.3 MiB (+47.46 %) | 24,138,134 | +38.69 % | 663 | 244 | 1637 |
| core (floor, non-functional) | 62,652,576 | +10.6 MiB (+21.63 %) | 20,988,672 | +20.59 % | 657 | 224 | 1431 |
| **min** (prototype) | 51,708,064 | **+0.19 MiB (+0.38 %)** | 17,462,407 | **+0.06 MiB (+0.33 %)** | 494 | 158 | 1063 |

¹ Same `go.mod` as `full`; the SOPS requirement is still listed.

Other effects:

- `full` grows `go.mod` from 220 to 308 lines and `go.sum` from 590 to 889 lines.
- `go get` upgrades shared modules that picolet already had: `google.golang.org/grpc` 1.78.0→1.82.1, `go.opentelemetry.io/otel` 1.39.0→1.44.0, `google.golang.org/api` 0.230.0→0.289.0, `github.com/ProtonMail/go-crypto` 1.3.0→1.4.1, `opencontainers/runc` 1.3.4→1.3.6, among others.
- `min` adds only `filippo.io/age` v1.3.1 and `filippo.io/hpke` v0.4.0 to the binary.

### M2: where the +23.4 MiB goes

These are symbol sizes from `go tool nm -size` on the unstripped `full` binary minus `upgr`. They cover code and data symbols but not the pclntab/rodata overhead, which roughly doubles the stripped delta. The proportions are still meaningful.

| Module group | Δ symbol bytes | Pulled in via |
|---|---:|---|
| gRPC xDS + envoy + CEL | +3.40 MB | `config` → `publish` → `cloud.google.com/go/storage` → `grpc/xds/googledirectpath` |
| AWS SDK v2 (kms, sts, s3, sso…) | +1.38 MB | `kms` (keyservice, config) and `publish` (S3) |
| Go runtime type metadata | +1.12 MB | all of the below |
| Google Cloud / googleapis | +0.98 MB | `gcpkms`, `publish` (GCS) |
| gRPC core | +0.69 MB | `keyservice` (a gRPC service), GCP clients |
| Azure SDK + MSAL | +0.39 MB | `azkv` |
| mongo-driver (bson) | +0.31 MB | `hckms` → `huaweicloud-sdk-go-v3/core` |
| OpenTelemetry | +0.24 MB | GCP clients |
| SOPS itself | +0.20 MB | — |
| Huawei Cloud SDK, tjfoc/gmsm | +0.16 MB | `hckms` |
| HashiCorp Vault API | +0.12 MB | `hcvault`, `publish` |
| filippo.io/age | +0.10 MB | `age` |

The shortest import chains, from `go mod why`:

- `decrypt → cmd/sops/common → kms → aws-sdk-go-v2/service/kms`
- `decrypt → config → azkv | gcpkms | hcvault | hckms → <SDK>`
- `decrypt → config → publish → aws-sdk-go-v2/service/s3 | cloud.google.com/go/storage`
- `cloud.google.com/go/storage → google.golang.org/grpc/xds`

### Why no narrower SOPS import works

- **Root package.** The root package `github.com/getsops/sops/v3`, which holds `Tree`, `Metadata` and `GetDataKey`, imports `keyservice` [S2]. `Metadata.GetDataKey()` always goes through `keyservice.NewLocalClient()`, whose server switches over every key type and imports `azkv`, `gcpkms`, `hckms`, `hcvault`, `kms` and `pgp` [S3]. The linker cannot prune them, because they are reachable from the type switch.
- **Stores.** `stores` (the metadata (de)serialisation) imports the same key sources [S5]. Every concrete store (`stores/yaml`, `stores/json`, which includes `BinaryStore`, `stores/dotenv`, `stores/ini`) imports `config` [S4]. `config` additionally imports `publish`, which brings S3, GCS and Vault [S4].
- **Build tags.** The module has no build constraints, apart from one `!windows` file in `cmd/sops/subcommand/exec`.
- **`aes`.** `aes` imports the root package, so even the cipher alone pulls the whole graph [S15].
- **Upstream view.** Commenters on #2157 trace the same coupling to `config/config.go`. One reports that a dependency-inversion attempt grew to 3,000+ changed lines. Another proposes build-tag-gated backends with a `sops_minimal` tag [S6].
- **Patched fork.** A `replace` to a patched SOPS fork that stubs the non-age key sources would work technically. It would also mean carrying a fork of a security-relevant module, so I did not pursue it.

### Key loading with the official API

`decrypt.Data` has no parameter for identities. The age key source loads them from the process environment and the home directory:

- `SOPS_AGE_KEY`
- `SOPS_AGE_KEY_FILE`
- `SOPS_AGE_KEY_CMD`
- `$XDG_CONFIG_HOME/sops/age/keys.txt`
- SSH keys via `SOPS_AGE_SSH_PRIVATE_KEY_FILE` / `_CMD` or `~/.ssh/id_ed25519` / `id_rsa`

[S16]. To pass identities explicitly, you have to drop `decrypt` and call `Metadata.GetDataKeyWithKeyServices` with a custom `keyservice` server that calls `age.ParsedIdentities.ApplyToMasterKey`. Flux's kustomize-controller does exactly that [S17]. Either way the size stays as in `full`/`narrow`.

### Third-party Go libraries

| Candidate | What it is | Independent of getsops? |
|---|---|---|
| `github.com/dcadolph/cipher` | "Programmatic SOPS for Go" CLI + library, 0 stars | No: requires `getsops/sops/v3` v3.13.2 [S8] |
| `github.com/trendvidia/sops/v4` | fork of SOPS (v4.1.0, 2026-07) | No: same cloud SDKs in `go.mod` [S9] |
| `warwick-mitchell1/sops`, `KoHcoJlb/sops/v3`, `YewFence/sops/v3`, `tozd/sops/v3` | forks | No [S10] |
| `choffmeister/kustomize-sops-decryptor` | tool, v0.1.0-alpha.3 (2021) | No: `go.mozilla.org/sops/v3` [S11] |
| `github.com/eiseron/sope` | TUI | No: `getsops/sops/v3` [S11] |

Outside Go:

- `sops-age` (npm, v4.0.2, 2025-06) is an independent TypeScript implementation built on `age-encryption` [S12].
- `File::SOPS` (Perl, CPAN) is another one [S13].

Both show the format can be reimplemented, but neither is usable from Go.

### What a minimal age-only reimplementation involves

The format as implemented in SOPS v3.13.3 (there is no written spec: the official docs describe it only as "values are encrypted using AES256_GCM" [S18], so the source is the authority):

1. **Data key.** A random 32-byte key [S2 `GenerateDataKey`]. For each age recipient it is age-encrypted and ASCII-armored into `sops.age[].enc`. Decryption is `age.Decrypt(armor.NewReader(enc), ids…)` [S16].
2. **Values.** Each value is `ENC[AES256_GCM,data:<b64>,iv:<b64>,tag:<b64>,type:<str|int|float|bool|bytes|time|comment>]` [S15]:
   - AES-256-GCM with a 32-byte nonce, so `cipher.NewGCMWithNonceSize`.
   - The AAD is the key path joined by `:` with a trailing `:`. Sequence items use their parent's path.
   - An empty string is stored as empty and is not encrypted [S2 `Tree.Decrypt`, S15].
3. **Which values are encrypted.** This depends on `unencrypted_suffix`, `encrypted_suffix`, `unencrypted_regex`, `encrypted_regex` and the comment regexes [S2 `shouldBeEncrypted`].
4. **MAC.**
   - It is SHA-512, written as uppercase hex.
   - It covers the canonical bytes of every leaf value in walk order (`ToBytes`: `True`/`False`, `strconv.FormatFloat(f,'f',-1,64)`, …). Unencrypted values are included, comments and nulls are not.
   - With `mac_only_encrypted`, it covers only encrypted values, after a fixed 32-byte prefix.
   - The MAC is itself stored encrypted, with AAD = `lastmodified` formatted RFC 3339 [S2, S19].
5. **Shamir.** Shamir key groups (`key_groups`, `shamir_threshold`) [S2 `GetDataKeyWithKeyServices`] are the one part with real cryptographic code. An age-only, single-group decryptor can refuse them.

**Effort.** The prototype is about 360 lines. It covers 1–4 for YAML and JSON input, multiple recipients, all four path rules and `mac_only_encrypted`, and is byte-identical to `sops -d` on the YAML fixtures. The remaining gaps:

- `time` values
- exact comment-regex semantics in sequences (SOPS treats comments as tree items [S2 `walkSlice`/`walkBranch`])
- YAML anchors
- dotenv/INI stores, which flatten metadata into `sops_*` keys or an `[sops]` section [S4]
- the binary store, which is the JSON store with one `data` key [S4] and would be easy
- SSH (`agessh`) and plugin identities
- Shamir

This is a few days of work including tests, not weeks.

**Drift risk.** It is moderate and visible:

- The file format has a backward-compatibility promise within the major version [S18], so already-written files stay readable.
- New features do show up in the format, though. Recent changelog entries [S19]:
  - `--mac-only-encrypted` (3.9.0)
  - `time.Time` values in YAML (3.10.0)
  - float/time string formatting for dotenv/INI (3.11.0)
  - hybrid post-quantum age identities (3.12.0, handled by `filippo.io/age` itself)
- A reimplementation would fail closed: on an unknown `type:` or a MAC mismatch it returns an error rather than wrong plaintext. The main risk is a file that a newer `sops` writes and the in-tree decryptor rejects.
- Golden fixtures from the real CLI, plus a CI job that re-encrypts them with each new SOPS release (picolet already runs Renovate), would catch that early.
- The security-relevant parts (age, AES-GCM, SHA-512) stay in maintained libraries or the stdlib.

### Formats each option decrypts

| Option | YAML | JSON | dotenv | INI | binary |
|---|---|---|---|---|---|
| `decrypt.Data` / `decrypt.File` [S20] | yes | yes | yes | yes | yes (default for unknown format strings) |
| narrow (SOPS stores + `aes`) | yes, with any store imported; same size | yes | yes | yes | yes |
| third-party Go libraries | none independent of SOPS | | | | |
| minimal reimplementation (prototype) | yes, verified byte-identical | decrypt + MAC verified, output re-emitted as YAML | not implemented (flat `sops_*` metadata) | not implemented (`[sops]` section) | not implemented (JSON store + `data` key, small step) |

### Data point: stryan/materia

The checked commit is `0d3aabd`, from 2026-10-23.

- It requires `github.com/getsops/sops/v3 v3.13.1` directly, plus `filippo.io/age v1.3.2` for its separate age engine. `go.mod` carries the full cloud-SDK indirect block, e.g. `cloud.google.com/go/storage` [S21].
- `pkg/attributes/sops/sops.go` calls `decrypt.File(path, filepath.Ext(path))`. Because `.yaml` is not a known format string, SOPS falls back to detecting the format from the path [S20]. It uses `cmd/sops/formats` (`IsYAMLFile`/`IsJSONFile`/`IsIniFile`), with a code comment that it "isn't technically stable". It accepts YAML, JSON and INI and skips dotenv and binary. Its docs say it expects YAML or INI [S21].
- It uses no SOPS-specific build tags or trimming, only the same Podman `remote …` tags as picolet with `-ldflags="-w -s"`. Keys come from the standard SOPS environment ("SOPS is configured externally") [S21].

So materia pays the full cost described above, deliberately or not.

### Artifacts (scratch, not committed)

- Prototype: [`_sops-age-prototype/sopsage.go`](_sops-age-prototype/sopsage.go) (a directory with a leading `_`, which the Go tooling skips; the fixtures are not committed because they contain throwaway private keys)
- Fixture script outputs: `sops-cost/fixtures/`
- Variant trees: `sops-cost/{base,upgr,full,narrow,core,min}`

## Sources

- [S1] Go module proxy, `github.com/getsops/sops/v3/@latest` → v3.13.3, 2026-07-23. https://proxy.golang.org/github.com/getsops/sops/v3/@latest
- [S2] SOPS v3.13.3 `sops.go`: import of `keyservice` (l. 57), `walkSlice`/`walkBranch` (l. 368–435), `shouldBeEncrypted` (l. 437), `Tree.Decrypt` (l. 587), `GetDataKeyWithKeyServices` (l. 842), `GetDataKey` (l. 965), `ToBytes` (l. 972). https://github.com/getsops/sops/blob/v3.13.3/sops.go
- [S3] SOPS v3.13.3 `keyservice/server.go`, imports l. 7–11, `decryptWithAge` l. 161. https://github.com/getsops/sops/blob/v3.13.3/keyservice/server.go
- [S4] SOPS v3.13.3 `config/config.go` (imports `azkv`…`kms`, `publish`, l. 16–22). `stores/yaml/store.go` l. 11 and `stores/json/store.go` l. 12 import `config`. `stores/json` `BinaryStore` uses a single `data` key (l. 30–82). `stores/dotenv`/`stores/ini` metadata flattening. https://github.com/getsops/sops/tree/v3.13.3/config, https://github.com/getsops/sops/tree/v3.13.3/stores
- [S5] SOPS v3.13.3 `stores/stores.go`, imports l. 20–24. https://github.com/getsops/sops/blob/v3.13.3/stores/stores.go
- [S6] getsops/sops#2157 "A slimmed version of sops with age encryption as a Go module?" (open, opened 2026-04-24, no maintainer reply as of 2026-10-24). https://github.com/getsops/sops/issues/2157
- [S7] getsops/sops#2016 "Proposal: out-of-tree encryption backends" (open), linked RFC PR #2210. https://github.com/getsops/sops/issues/2016
- [S8] `dcadolph/cipher` repository and `go.mod`. https://github.com/dcadolph/cipher
- [S9] `github.com/trendvidia/sops/v4` v4.1.0 `go.mod`. https://proxy.golang.org/github.com/trendvidia/sops/v4/@v/v4.1.0.mod
- [S10] pkg.go.dev search "sops age" (14 results, all SOPS forks/wrappers or tools). https://pkg.go.dev/search?q=sops+age&m=package
- [S11] `go.mod` files: https://proxy.golang.org/github.com/choffmeister/kustomize-sops-decryptor/@v/v0.1.0-alpha.3.mod, https://proxy.golang.org/github.com/eiseron/sope/@v/v0.4.0.mod
- [S12] npm registry `sops-age` (v4.0.2, 2025-06-03; deps `age-encryption`, `@noble/ciphers`, …). https://registry.npmjs.org/sops-age, https://github.com/humphd/sops-age
- [S13] CPAN `File::SOPS::Encrypted`. https://metacpan.org/pod/File::SOPS::Encrypted
- [S14] picolet `Containerfile` and `Taskfile.yml` (build tags, `-s -w`, `-trimpath`), in this repository.
- [S15] SOPS v3.13.3 `aes/cipher.go`: `nonceSize = 32` (l. 34), value regex (l. 55), `Decrypt` (l. 80), imports root `sops`. https://github.com/getsops/sops/blob/v3.13.3/aes/cipher.go
- [S16] SOPS v3.13.3 `age/keysource.go`: env variables (l. 29–45), `ApplyToMasterKey` (l. 157), `Decrypt` with armor reader (l. 243), `loadIdentities` (l. 411). https://github.com/getsops/sops/blob/v3.13.3/age/keysource.go
- [S17] Flux kustomize-controller `internal/sops/keyservice/server.go` (`ageIdentities.ApplyToMasterKey`). https://github.com/fluxcd/kustomize-controller/blob/main/internal/sops/keyservice/server.go
- [S18] SOPS docs: backward compatibility of the file format (https://getsops.io/docs/) and security model, "Values are encrypted using AES256_GCM" (https://getsops.io/docs/security/).
- [S19] SOPS v3.13.3 `CHANGELOG.md`: 3.9.0 `--mac-only-encrypted` (#973), 3.10.0 `time.Time` (#1759), 3.11.0 float/time formatting for dotenv/INI (#1929), 3.12.0 hybrid post-quantum age identities (#2033). https://github.com/getsops/sops/blob/v3.13.3/CHANGELOG.md
- [S20] SOPS v3.13.3 `decrypt/decrypt.go` (only package with a stable API; `Data` accepts `json`, `yaml`, `ini`, `dotenv`, `binary`) and `cmd/sops/formats/formats.go` (`FormatFromString` defaults to binary, `FormatForPathOrString` falls back to the extension). https://github.com/getsops/sops/blob/v3.13.3/decrypt/decrypt.go, https://github.com/getsops/sops/blob/v3.13.3/cmd/sops/formats/formats.go
- [S21] stryan/materia at `0d3aabd`: `go.mod`, `pkg/attributes/sops/sops.go`, `mise.toml` (build tags, `-w -s`), `docs/markdown/attributes.md`. https://github.com/stryan/materia
