# What can `picolet validate` check about a SOPS reference without the age key? (#240)

## Answer

- **Without the key, CI can prove that a reference resolves to an encrypted leaf, and can read the leaf's type.** In YAML, JSON and dotenv files, key names, nesting, list lengths and order stay in cleartext; only leaf values are encrypted [S1][S4]. Each encrypted leaf is a self-describing string, `ENC[AES256_GCM,data:…,iv:…,tag:…,type:str|int|float|bool|time|comment]` [S2]. A keyless loader can therefore:
  - resolve `sops://<fleet-path>#<key.path>`, reporting "not found" or "index out of bounds";
  - reject a reference to a mapping or list instead of a leaf;
  - tell `ENC[…]` apart from a value left plaintext by `unencrypted_*`/`encrypted_*` rules (the rules are recorded in the file's `sops:` block);
  - flag a value that the file's own rules say must be encrypted but is plaintext. That is a hand edit, and `sops decrypt` on the Host would fail on it [S3].
  The `type:` tag and the ciphertext length (equal to the plaintext byte length) are visible. Empty strings and `null` are never encrypted [S2][S3].
- **CI can compare the file's age recipients against a Host's public key, but the check only catches honest mistakes.** The `sops:` metadata lists every age recipient in cleartext, either under `age:` or under `key_groups[].age` with `shamir_threshold` [S5]. The `recipient:` field is a label only: it is not covered by the MAC, and the age stanza does not contain the recipient's public key (an X25519 stanza carries only an ephemeral share) [S6][S7]. sops ignores the label when it decrypts; it tries every identity it has [S7]. I relabelled a stanza and dropped another; both edits went undetected [Method, T1/T2]. So the check catches "a Host was added but `sops updatekeys` was never run", not a malicious edit. With several key groups, the Host's key must appear in at least `shamir_threshold` groups (when the threshold is 0, every group is required) [S8].
- **The MAC and the ciphertexts cannot be checked without the key (confirmed).** The MAC is SHA-512 over the plaintext values, stored AES-GCM-encrypted under the data key with `lastmodified` as AAD [S3][S9]. Each value's AAD is its key path [S1][S3]. Checking either one needs the data key. Without the key, CI sees only shape. A forged `ENC[…,type:str]` value passes a keyless check and fails on the Host [Method, T10]. The `sops:` metadata itself (recipients, rules, `version`) is not authenticated at all: changing `version` still decrypts [T9]. `sops filestatus` is keyless, but it checks only that `version` and `mac` exist [S10].
- **Today, providers without credentials either use placeholders or are skipped.** `validate` and `resolve` build the resolver with no provider readers (`pkg/cli/runners.go:317`, `:330`).
  - In templates, a provider without a reader returns `PlaceholderEmpty` (`<op-secret>`/`<pp-secret>`), and its refs are never collected (`pkg/resolver/registry.go:288-295`). `PlaceholderPending` appears only in the collect pass of a configured provider (`registry.go:299-302`).
  - Direct `secrets:` refs are logged as skipped and dropped from the file set (`resolver.go:794-805`, `:510-513`), so the validator never sees them.
  - `Strict` turns both cases into errors (`registry.go:291-293`, `resolver.go:799-801`). Only bootstrap sets it (`pkg/bootstrap/resolve.go:61`).
- **A structural SOPS check fits in the resolver, not the validator.** The encrypted file is in the Fleet FS the resolver already holds, so in keyless mode a `sops` provider can have a non-nil reader that inspects instead of decrypting. It would return a placeholder per valid ref and an error per bad one, with templates and direct refs running through the existing paths (`registry.go:288-318`, `resolver.go:763-812`). A recipient check needs a per-Host input, analogous to `validator.Target.HostDataDir` (`pkg/validator/validator.go:25-33`). `AnalyzeFiles`' per-category hook (`validator.go:57-59`) is the precedent if the check should report through the validator instead. Note that `sops/v3/stores` imports every KMS backend: my keyless probe was 64 MB and pulled 1015 packages [T11].

## Method

All experiments ran under the scratchpad (`…/scratchpad/sops-validate/`). Nothing was changed in the picolet checkout.

- Tools: `sops 3.13.3` (`go install github.com/getsops/sops/v3/cmd/sops@latest`) and `age`/`age-keygen`/`age-inspect` v1.3.2 (`filippo.io/age/cmd/...@latest`). Go 1.27.
- Three throwaway X25519 identities, labelled `srv-1`, `srv-2` and `srv-3` (public keys only shown here):
  - `srv-1`: `age15xstlzrqhj3wfmkh80ux9r3aqsw2c3mw075t5mhapeuz484gma3q9s4j7w`
  - `srv-2`: `age146j2z9qy65etlqpyuqmqrtefeeh833nrmtya9k4ykjl8znxnqs5qdwkjxn`
  - `srv-3`: `age1r4n54uhn66sngsqzw8pcny5x93d3j9c3q9kqeae3sk4km5g2yc6qdunrtu`
- Samples (YAML with nested maps, a list of scalars and maps, int/bool/float/empty/null and comments; JSON; dotenv) were encrypted to `srv-1,srv-2`. Variants used `--encrypted-regex`, `--unencrypted-regex` and `--encrypted-suffix`, plus a `.sops.yaml` with `mac_only_encrypted: true` and one with three key groups and `shamir_threshold: 2`.
- Decryption ran with `HOME` pointed at an empty dir and no `SOPS_AGE_KEY*` set (keyless), then with `srv-1` (a recipient) and `srv-3` (not a recipient).
- Tampering tests, decrypted with `srv-1` unless noted:

| # | Edit to the encrypted file | Result |
|---|---|---|
| T1 | `srv-2`'s `recipient:` label replaced by `srv-3`'s key | `srv-3`: data-key failure; `srv-1`: **OK** |
| T2 | `srv-2`'s age stanza removed | `srv-1`: **OK**; `srv-2`: data-key failure |
| T3 | `note_unencrypted: visible` → `changed` | `MAC mismatch` |
| T3b | same, file with `mac_only_encrypted: true` | **OK** |
| T4 | ciphertexts of `db.user` and `db.password` swapped | `cipher: message authentication failed` (AAD = path) |
| T4b | ciphertexts of `tokens[0]` and `tokens[1]` swapped | `MAC mismatch`. With `--ignore-mac`, `tokens[0]` decrypts to `beta`: list indices are not in the AAD |
| T5 | `lastmodified` changed | `Cannot decrypt MAC: … authentication failed` |
| T6 | `type:str` → `type:int` on `db.password` | `strconv.Atoi: parsing "hunter2"` |
| T7 | plaintext key `extra: injected` added (no suffix) | `Input string injected does not match sops' data format` |
| T8 | `unencrypted_suffix` changed in metadata | same "does not match sops' data format" on the formerly plaintext key |
| T9 | `version: 3.13.3` → `3.0.0` | **OK** (metadata not authenticated) |
| T10 | `fake: ENC[AES256_GCM,data:AAAA,iv:AAAA,tag:AAAA,type:str]` added | keyless probe: "ENCRYPTED type=str"; decrypt: `message authentication failed` |

- T11, a keyless probe: a 150-line Go program that loads files with `sops/v3/stores/{yaml,json,dotenv}` `LoadEncryptedFile` (no key involved). It prints the metadata and recipients and resolves dotted key paths with `TreeBranch.Truncate`. It also re-applies the suffix/regex rules, because `Tree.shouldBeEncrypted` is unexported [S3]. Build result: 1015 packages in `go list -deps` (AWS, GCP and Azure SDKs, Vault, gRPC), and a 64 MB binary against a 2.4 MB hello-world. The cause is that `stores/stores.go` imports `azkv`, `gcpkms`, `hckms`, `hcvault`, `kms` and `pgp` [S5].

## Details

### 1. What stays plaintext

**YAML** (default rules, trimmed; age armour bodies elided):

```yaml
db:
    user: ENC[AES256_GCM,data:mcSS,iv:zSCj…,tag:5UeX…,type:str]
    password: ENC[AES256_GCM,data:KcRO7tuPOQ==,iv:I5co…,tag:0UE1…,type:str]
    port: ENC[AES256_GCM,data:NV1Jew==,iv:…,tag:…,type:int]
    tls: ENC[AES256_GCM,data:Df/75Q==,iv:…,tag:…,type:bool]
    ratio: ENC[AES256_GCM,data:yBvv,iv:…,tag:…,type:float]
    empty: ""
    nothing: null
tokens:
    - ENC[AES256_GCM,data:MUw4KK0=,iv:…,tag:…,type:str]
    - ENC[AES256_GCM,data:dJPx6Q==,iv:…,tag:…,type:str]
    - name: ENC[AES256_GCM,data:Au9/znrG,iv:…,tag:…,type:str]
      value: ENC[AES256_GCM,data:I/axrpE=,iv:…,tag:…,type:str]
note_unencrypted: visible
sops:
    age:
        - enc: |
            -----BEGIN AGE ENCRYPTED FILE-----
            …
            -----END AGE ENCRYPTED FILE-----
          recipient: age15xstlzrqhj3wfmkh80ux9r3aqsw2c3mw075t5mhapeuz484gma3q9s4j7w
        - enc: |
            …
          recipient: age146j2z9qy65etlqpyuqmqrtefeeh833nrmtya9k4ykjl8znxnqs5qdwkjxn
    lastmodified: "2026-10-24T18:32:18Z"
    mac: ENC[AES256_GCM,data:KN4D…,iv:…,tag:…,type:str]
    unencrypted_suffix: _unencrypted
    version: 3.13.3
```

- **Key names, nesting, key order, list length and order, and maps inside lists** are all cleartext. The sops docs say: "keys are stored in cleartext, and values are encrypted" [S4]. The encryption protocol says key names are kept in cleartext "for better diff and overall readability" [S1].
- **Value type** is visible through the `type:` tag (`str`, `int`, `float`, `bool`, `time`, `comment`) [S2]. **Value length** is visible as well: GCM's ciphertext length equals the plaintext length, and the tag is stored separately (`data:KcRO7tuPOQ==` is 7 bytes, the length of `hunter2`) [S2].
- **Empty strings and `null` stay as they are.** The cipher returns `""` for empty input [S2], and the tree walk skips `nil` [S3]. So "this key exists and is `ENC[…]`" fails for an empty secret.
- **Comments are encrypted** unless a comment regex says otherwise. In YAML a comment becomes `#ENC[…,type:comment]` [S3][S11]; observed for both whole-line and inline comments.
- **Partial-encryption rules are recorded in the metadata**, as whichever one was used: `unencrypted_suffix` (sops writes the default `_unencrypted` when nothing else is chosen [S3]), `encrypted_suffix`, `unencrypted_regex`, `encrypted_regex`, `unencrypted_comment_regex` or `encrypted_comment_regex`, plus `mac_only_encrypted: true` when set [S5][S11]. Each rule matches on any key name along the path, so a matching ancestor decides for its whole subtree [S3]. Observed: `encrypted_regex: ^(password|value)$` left `db.user`, `db.port` and `tokens[0]` plaintext but encrypted `tokens[2].value`. In 3.13.3 `--mac-only-encrypted` is a global flag (`sops --mac-only-encrypted -e …`), not a `sops encrypt` flag, although the reference docs call it a flag [S9][S12].
- **`lastmodified`, `mac` (present, encrypted) and `version`** are cleartext [S5].
- **Key groups**: with `.sops.yaml` `key_groups` + `shamir_threshold: 2`, the metadata is `key_groups: [{age: [{enc, recipient}]}, …]` plus `shamir_threshold: 2`, all cleartext.

**JSON**: same structure. Every encrypted leaf becomes a JSON *string* `"ENC[…,type:int]"`, so the original JSON type survives only in the tag. The `sops` object has the same keys.

**dotenv**: flat, so there is no nesting. Values, including numbers (`DB_PORT=ENC[…,type:str]`), become `type:str`, and comments become `#ENC[…,type:comment]`. The metadata is flattened into prefixed keys [S13]:

```
DB_PASSWORD=ENC[AES256_GCM,data:qeR1Ii/ByQ==,iv:…,tag:…,type:str]
#ENC[AES256_GCM,data:kt6Nd+ZqSYolcA==,iv:…,tag:…,type:comment]
NOTE_unencrypted=visible
sops_age__list_0__map_enc=-----BEGIN AGE ENCRYPTED FILE-----\n…
sops_age__list_0__map_recipient=age15xstlzrqhj3wfmkh80ux9r3aqsw2c3mw075t5mhapeuz484gma3q9s4j7w
sops_age__list_1__map_recipient=age146j2z9qy65etlqpyuqmqrtefeeh833nrmtya9k4ykjl8znxnqs5qdwkjxn
sops_lastmodified=2026-10-24T18:32:18Z
sops_mac=ENC[AES256_GCM,data:…,type:str]
sops_unencrypted_suffix=_unencrypted
sops_version=3.13.3
```

### 2. Key path exists, is encrypted, type

The probe (T11) output for the YAML sample, run without any key:

```
ref db.password: ENCRYPTED type=str; rules say encrypted=true
ref db.port: ENCRYPTED type=int; rules say encrypted=true
ref db.empty: empty string (sops never encrypts it); rules say encrypted=true
ref db.missing: NOT FOUND (component ['missing'] not found)
ref tokens.5: NOT FOUND (component [5] accesses out of bounds)
ref note_unencrypted: PLAINTEXT string "visible"; rules say encrypted=false
ref db: mapping with 7 keys; rules say encrypted=true
```

Against `tamper/add.yaml` (T7): `ref extra: PLAINTEXT string "injected"; rules say encrypted=true`. That file will not decrypt on the Host, so CI catches it early.

Checks a keyless `sops://` validator can make, each one tied to the source behaviour:

1. **The file is a SOPS file**: it parses in the format chosen by extension and has a `sops` key with `version` and `mac`. This is the same rule `sops filestatus` uses [S10]. SOPS picks the format from the extension [S4], so `sops://` should do the same or carry an explicit format.
2. **The path exists and ends at a leaf.** This uses `TreeBranch.Truncate`, the function behind `sops decrypt --extract` [S3].
3. **The leaf is encrypted**: it matches `^ENC\[AES256_GCM,data:(.+),iv:(.+),tag:(.+),type:(.+)\]` [S2] and is not `""`/`null`. A leaf the rules leave plaintext means the "secret" is cleartext in git, which is a likely error.
4. **File consistency**: every leaf that `shouldBeEncrypted` [S3] marks as encrypted is `ENC[…]`, `""` or `null`. Otherwise decryption on the Host fails (T7, T8). This needs a re-implementation of the unexported rule function (about 80 lines including the comment-regex rules).
5. **Type**: the `type:` tag, for example to reject `comment`, or to warn when a template expects a string.

Grammar caveats for `#<key.path>`: list items have no key name, so a path segment must allow indices (`tokens.2.value`). YAML keys may contain `.`, which is why `sops --extract` uses `["a"]["b"]` [S12]. A dotenv path is a single key. A YAML stream with several documents gives several branches, and `--extract` reads the first [S3].

### 3. Comparing recipients with a Host's age public key

**What can be compared.** For each age master key, `stores` loads `{recipient, enc}` [S5] into `age.MasterKey.Recipient`, a Bech32 `age1…` string. It can also be `age1pq1…` (hybrid post-quantum), a plugin recipient, or `ssh-ed25519/ssh-rsa` [S7]. Recipients can be compared to a Host key by exact string match; age emits lowercase Bech32. The rule for "can this Host decrypt" comes from `GetDataKeyWithKeyServices` [S8]:
- With one key group, any one recipient in the group is enough.
- With more than one group, the Host must decrypt at least `shamir_threshold` groups. When the threshold is 0, sops sets it to the number of groups at encryption time [S8].
- Observed: with three groups and threshold 2, `srv-1` alone fails.

**Limit: the label is not proof.** The X25519 stanza is `-> X25519 <ephemeral share>` plus a wrapped key [S6]. `age-inspect` reports only "recipient types: X25519", so the public key cannot be recovered from `enc`. `MasterKey.Decrypt` calls `age.Decrypt(armor, identities...)` without using `Recipient` [S7], and the MAC covers values only [S3][S9]. T1 and T2 confirm this: a relabelled or removed stanza goes unnoticed by both CI and `srv-1`. In practice sops writes the label from the key it actually wrapped to. So the comparison reliably catches drift (a new Host whose file was not re-keyed), but not a hostile edit, and a stanza's label could still be wrong after manual editing.

**Where a Host's public key could live in a Fleet** (options, not a decision):

1. **`hosts/<name>/host.yml`, a new key** (for example `age_recipient: age1…`, or a list to allow rotation). `HostConfig` (`pkg/config/host.go:31-64`) is loaded strictly (`config.go:200`, `WithKnownFields`). Older Agents tolerate the new key only through `LenientHosts` (`config.go:65-72`, `:173-191`); `validate`, `resolve` and `bootstrap` fail on it until they are upgraded. It sits next to `machine:`/`user:`, which fits "one identity per Host".
2. **A file beside it**, such as `hosts/<name>/age.pub` (or `age-recipients.txt`, one per line, as `age -R` reads). There is no schema change and no impact on older Agents, but it is outside the typed config.
3. **`fleet.yml`**, a map from Host (or Machine) to recipient. `FleetConfig` (`pkg/config/fleet.go:10-20`) is strict too. One file lists all keys, which is convenient for generating `.sops.yaml`, but the key sits away from the Host.
4. **Per Machine instead of per Host**: one key shared by every Host on a Machine. This weakens the Host as the runtime blast-radius boundary (CONTEXT.md "Host"; ADR 0002).

**ADR to surface:** `docs/adr/0002-host-per-linux-user.md` accepts that "every Linux user on a Machine can read the whole Fleet" because "nothing secret may ever be in git". SOPS ciphertext in git is a deliberate exception to that sentence. Its safety then rests on per-Host recipients, which makes the recipient check above more valuable, and the ADR (or a new one) should record the exception.
5. **Derive from `.sops.yaml` `creation_rules`**. The SOPS-native list maps `path_regex` to recipients, not Host to recipients, so picolet would still need the Host side from 1 to 4. A check that the file's recipients match what `.sops.yaml` would produce is what `sops updatekeys` does.

The private identity has no Fleet place. It would sit beside the Agent config like `onepassword.token_path` (README "Secret Providers"). `bootstrap machine` could place it as a bootstrap credential, but today `BootstrapFiles.validate` accepts only `op://`/`pass://` sources (`pkg/config/bootstrap.go:31-42`).

### 4. MAC and other key-dependent checks

- The MAC is SHA-512 over each value's bytes in walk order: plaintext values too unless `mac_only_encrypted` is set, and never comments. It is hex-encoded, then AES-GCM-encrypted with the data key and AAD = `lastmodified` (RFC 3339) [S3][S9][S14]. Verification decrypts the tree, recomputes the hash and compares [S14]. **Neither the stored MAC nor the hash input is available without the data key.** Confirmed by T3, T4b and T5.
- Each value is sealed with AAD = the `:`-joined key path plus `:`, so moving a value between keys fails (T4) [S1][S3]. List indices are not part of the path (`walkSlice` passes `path` unchanged [S3]), so swaps inside a list are caught only by the MAC (T4b). All of this needs the key.
- Decrypting `enc` needs a recipient identity, so even the stanza count is all CI can check about it.
- **What is left without the key**: format parsing (base64 of `data`/`iv`/`tag`, known `type`), recipient labels, metadata shape. None of this is integrity.

### 5. Today's provider behaviour without credentials, and plug-in points

How `validate`/`resolve` wire providers:
- `runValidate` (`pkg/cli/runners.go:311-322`) and `runResolve` (`:324-330`) build `resolver.New(resolver.Config{FS: repo.FS, Config: repo.Config})`: no `SecretReader`, no `OpSecretReader`/`PPSecretReader`, `Strict: false`. The same holds for `dry-run` without `--config` (`dryRunResolveBasic`, `:238-255`).
- With `--config`, dry-run builds real readers (`dryRunResolveWithConfig`, `:201-236`; `opReaderFromConfig` `:172-181`).
- The Agent passes its readers and `LenientHosts: true` (`pkg/agent/agent.go:660-664`).
- Bootstrap is the only caller with `Strict: true` (`pkg/bootstrap/resolve.go:55-63`).

How the resolver behaves with a nil reader:
- Each Host's registry gets `OpProvider(r.opSecretReader)` and `PPProvider(r.ppSecretReader)` (`pkg/resolver/resolver.go:226-229`). `provider()` copies `r.strict` into them (`:277-280`).
- `ProviderTemplate` (`registry.go:60-68`) holds `FuncName`, `Reader`, `IsRef`, `PlaceholderEmpty`, `PlaceholderPending` and `Strict`. `OpProvider`/`PPProvider` define `<op-secret>`/`<op-secret-pending>` and `<pp-secret>`/`<pp-secret-pending>` (`:72-94`). `readSecretFile` returns `<secret>` when there is no `SecretReader` (`:96`, `:194-199`).
- `registerProviderFunc` (`registry.go:288-318`) behaves as follows:
  - with no reader (`cache == nil`), it returns `PlaceholderEmpty`, or an error under `Strict` (`:290-295`). **It does not validate the ref (`IsRef` comes after) and does not collect it.**
  - with a reader, it rejects an invalid ref (`:296-298`), collects and returns `PlaceholderPending` during the first pass (`:299-302`), and returns the resolved value or resolves a cache miss on its own (`:303-317`).
  - Caches are created only for providers with a non-nil reader (`:168-173`). The collect pass runs only when caches exist (`resolver.go:329-338`).
- Direct `secrets:` refs: `batchResolveDirectSecrets` → `resolveProviderRefs` (`resolver.go:763-812`). With a nil reader it errors under `strict` (`:799-801`), otherwise it logs "skipping secrets (provider not configured)" (`:802-803`). `buildFiles` then `continue`s past the ref (`:510-513`), so **no `ResolvedFile` reaches the validator**. Only `secretDestPath` parses the ref, and it does so before that, for collision detection (`:470-485`; `expandAndValidate` → `buildFileSkeletons` → `destPath`, `:380-393`, `:430`).
- In the validator, `validateSecret` requires non-empty content and YAML-checks `.yml`/`.yaml` sources except the `<secret>` placeholder (`pkg/validator/validator.go:281-300`, constant at `:22`).

Where a structural SOPS check could plug in. These are options; none is a recommendation.

- **A. Keyless reader in the resolver** (closest to the current design). Add a third `ProviderTemplate` (for example `readSopsSecret`) and a `sops://` branch in `isProviderRef`/`splitDirectRefs`/`secretDestPath` (`resolver.go:529-531`, `:782-792`, `:470-485`).
  - In `validate`/`resolve`, wire a non-nil `SecretRefReader` that opens `<fleet-path>` from `r.fsys` (the `os.Root`-backed `config.OpenRepo` FS, which CLAUDE.md requires so that a path cannot escape the tree), runs the checks in §2, and returns a placeholder such as `<sops-secret>` per valid ref.
  - Errors then surface on paths that already exist: in templates, as a final-render error with the template name; for direct refs, as a `resolving sops secrets:` error. Both reach `ValidateHost` as `host X: resolve: …` (`validator.go:68-79`).
  - Because the reader is non-nil, template refs are collected and checked, unlike `op://`/`pass://` today. The decrypting reader takes the same slot on the Agent.
  - Watch for: a validate-mode placeholder must not count as a value for `validateSecret`'s YAML check (`validator.go:295-300`). `recordProviderRefCount` would need a `sops` row (`pkg/agent/agent.go:704-705`).
- **B. Validator pass over references.** `ResolvedHost` would carry the Host's `sops://` refs, and the validator would need FS access (today it gets only `[]ResolvedFile` + `Target`, `validator.go:36-65`). `AnalyzeFiles`' per-category post-check (`:57-59`, `checkBuild`) is the precedent. A recipient check needs a Host field in `Target` (`:25-33`), filled by the caller from the resolved Host, the same way `HostDataDir` is.
- **C. Config-load check** for direct refs only, like `BootstrapFiles.validate` (`pkg/config/bootstrap.go:31-42`). It cannot see template refs, which are known only after rendering.

The recipient check (§3) needs the Host's public key next to `hostname` wherever it runs. In A that is `HostConfig` (already in `ResolvedHost.Host`); in B it is `Target`.

**Dependency note (fact, not a recommendation):** `github.com/getsops/sops/v3/stores` imports every key backend [S5]. A keyless loader built on it pulls about 1000 packages and grows a binary by roughly 60 MB (T11). The keyless format is small: a YAML/JSON/dotenv tree plus a regex [S2]. For comparison, picolet's `go.mod` already has `go.yaml.in/yaml/v4` and no `filippo.io/age`.

## Sources

Code is cited at sops `v3.13.3` (module cache `github.com/getsops/sops/v3@v3.13.3`), `filippo.io/age@v1.3.2`, and the sops docs repo (`github.com/getsops/docs`, commit `b2aae9d`, 2026-10-21). picolet file:line refers to the checkout under review.

- [S1] sops docs, `content/en/docs/reference/_index.md`, "Encryption Protocol" (lines 487-559): leaf values AES256_GCM, unique IV per value; "key names are concatenated into a byte string that is used as AEAD additional data"; keys kept in cleartext. Also "YAML anchors" (line 98): "SOPS uses the path to a value as additional data".
- [S2] sops `aes/cipher.go`: `encre` regex (line 55), `parse` (57-77); `Decrypt` returns `""` for empty input (80-83); `isEmpty` (127-139); `Encrypt` returns `""` for empty, type tags `str/int/float/bool/time/comment`, output `ENC[AES256_GCM,data,iv,tag,type]` with tag split off (141-198).
- [S3] sops `sops.go`: `Truncate` (309); `walkValue` skips `nil` (359-362); `walkSlice` does not extend `path` (368-388); `walkBranch` appends key (391-434); `shouldBeEncrypted`, any path element decides (437-509); `Encrypt`: MAC hashes all values unless `MACOnlyEncrypted`, comments excluded, AAD `strings.Join(path, ":") + ":"` (523-577, 549); `Decrypt`: a non-`ENC` string where encryption is expected → "Could not decrypt value" (587-650); `DefaultUnencryptedSuffix = "_unencrypted"` (64).
- [S4] sops docs, `reference/_index.md`, "YAML, JSON, ENV and INI type extensions" (lines 9-20): trees, only leaf values encrypted, format from extension. getsops.io/docs/security/ "Operational requirements": "keys are stored in cleartext, and values are encrypted".
- [S5] sops `stores/stores.go`: on-disk `metadata` struct with `key_groups`, `shamir_threshold`, `age`, `lastmodified`, `mac`, the `*_suffix`/`*_regex` fields, `mac_only_encrypted`, `version` (37-57); `agekey{recipient, enc}` (106-109); imports of all KMS backends (lines 3-17).
- [S6] age `x25519.go` `(*X25519Recipient).Wrap` (64-90): stanza `Type: "X25519", Args: [ephemeral public key]`; recipient key only in the HKDF salt. Observed with `age-inspect` and by decoding the armour: `-> X25519 <share>`.
- [S7] sops `age/keysource.go`: `Recipient` field (70-71); `MasterKey.Decrypt` uses `age.Decrypt(ar, key.parsedIdentities...)` without the label (243-272); `parseRecipient` accepts `age1pq1…`, plugin `age1…1…`, `age1…`, `ssh-…` (489-515).
- [S8] sops `sops.go`: `ShamirThreshold == 0` → `len(KeyGroups)` (782-783); `GetDataKeyWithKeyServices`: more than one group needs at least the threshold number of parts, a single group needs exactly one (842-878).
- [S9] sops docs, `reference/_index.md` "Message Authentication Code" (561-569); "Settings": `mac_only_encrypted`, `shamir_threshold`, the suffix/regex keys, "at most, one of these keys can be used", "Comments are always encrypted" (403-439).
- [S10] sops `cmd/sops/subcommand/filestatus/filestatus.go` `cfs` (40-60): encrypted only if metadata is found and `Version` and `MessageAuthenticationCode` are non-empty.
- [S11] Observed in samples: YAML comments become `#ENC[…,type:comment]`; `encrypted_regex`, `unencrypted_regex`, `encrypted_suffix` and `mac_only_encrypted` are written into `sops:`.
- [S12] sops `cmd/sops/main.go`: global flag `mac-only-encrypted` (1818) read via `c.GlobalBool` (2119); `sops encrypt --mac-only-encrypted` → "flag provided but not defined" (observed). `--extract` syntax per `sops decrypt --help`.
- [S13] sops `stores/dotenv/store.go`: `MetadataFlattenFull` (33, 79); `SopsPrefix` keys (139).
- [S14] sops `cmd/sops/common/common.go`: `DecryptTree` decrypts the MAC with AAD `LastModified.Format(time.RFC3339)` and compares (85-105); encrypting the MAC on write (126-127). `decrypt/decrypt.go` (52-70): same verification.
- picolet: `pkg/resolver/registry.go`, `pkg/resolver/resolver.go`, `pkg/cli/runners.go`, `pkg/validator/validator.go`, `pkg/bootstrap/resolve.go`, `pkg/agent/agent.go`, `pkg/config/{host,config,fleet,bootstrap}.go`, at the lines given inline. `CONTEXT.md` ("Host", "Secret Reference"), `docs/adr/0002-host-per-linux-user.md`, README "Secret Providers" and "Validation".
