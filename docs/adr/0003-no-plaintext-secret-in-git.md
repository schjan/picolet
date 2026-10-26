---
status: accepted
---

# No plaintext secret in git: SOPS/age values in the Fleet, one age identity per Host

Amends two clauses of [ADR 0002](0002-host-per-linux-user.md): "nothing secret may ever be in git", and the default of one secret-provider token per Machine. ADR 0002 stays `accepted` and carries an "Amended by 0003" note.

Some Fleets can't use a hosted secret manager (no budget, or Hosts that reach the git server but not the internet), and a self-hosted one is a service that has to run outside the Fleet. SOPS with age needs no service at all. We decided that **a secret value may be committed to the Fleet only inside a SOPS file encrypted to age Recipients**, named `*.sops.*` and reached only through a `sops://` Secret Reference; nothing else secret enters git in plaintext, ever. `sops://` stands beside `op://` and `pass://` with no preference, mixed freely per Host and per value. Only the Agent decrypts: `validate`, `resolve` and `bootstrap` stay keyless.

Each Host gets **its own age identity**, the rootful Host included, which is stricter than ADR 0002's per-Machine token. Every Linux user on a Machine can read the whole checkout, so inside it the Recipients are the only boundary between Hosts; one key per Machine would let a Host that runs foreign code (a CI runner) decrypt its neighbour's values. Each file is encrypted only to the Hosts whose Agents consume it plus declared Operators (one file per audience), and `validate` enforces that.

## Consequences

- **What stays public.** Everything SOPS leaves in plaintext is public to every reader of the Fleet: key names and structure, the type and length of every value, list lengths, the `sops:` metadata block (each Recipient's public key, the last-modified time, the SOPS version), and values left unencrypted by `unencrypted_*`/`encrypted_*`/`mac_only_encrypted` rules. The old rule otherwise stands: no plaintext tokens, passwords, private keys or key-bearing URLs, and no file a secret was ever committed to.
- **Ciphertext is permanent.** It counts as safe against every reader of the Fleet, a public mirror included. Fleets with a public repository are recommended to use hybrid post-quantum recipients.
- **Leaks are fixed by rotation, never by rewriting history.** Both of these are rotation events: a plaintext secret in any reachable commit (including one later replaced by its encrypted form), and a value encrypted to a Recipient that shouldn't read it.
- **Removing a Recipient.** `sops updatekeys` re-wraps the file's existing data key, so a removed Recipient can still unwrap that key from any older commit.
  - *Trust lost* (a person leaves, a Machine is lost or stolen, an identity is exposed): drop the Recipient, give the file a new data key (`sops rotate`), and change every value ever encrypted to that Recipient, in that commit or a later one, never before.
  - *Planned retirement* (an identity rotation, or a decommissioned Host whose identity is destroyed everywhere, the `--secrets-dir` backup included): dropping the Recipient and running `sops updatekeys` is enough.
- **Confidentiality, not authenticity.** Encrypting needs only public recipients, so anyone with git write access can commit a valid SOPS file, and the MAC is no guard against a malicious commit. A git writer can also commit a template that renders any value a Host can decrypt into a place the writer can read. Git write access is therefore the trust boundary for both desired state and secret values. The `validate` recipient checks compare unauthenticated labels: they catch drift, not a hostile edit. Keeping CI keyless is a security property, not a limitation: a CI job never holds an identity.
- **Fetch credentials are never SOPS values.** The git token and the GitHub App credentials stay Bootstrap credentials (a `--secrets-dir` file, or a `bootstrap:` entry from `op://`/`pass://`). The Agent has no checkout to decrypt from on first start, and a rotation would strand every Agent that hasn't fetched the new commit yet. ADR 0002's "the running Agent resolves its git credentials … through references" therefore never means `sops://`. This costs no security: the age identity reaches the Host the same way, so a token stored next to it is just as well protected.
- **Repairability.** SOPS runs no service, so only the git server and the Host's identity file matter for repairing a Host.
- **A Host that runs foreign code** (CI jobs control that user's Podman) must treat its identity as exposed to that code, and must never share a file's audience with a Host that runs no foreign code.
