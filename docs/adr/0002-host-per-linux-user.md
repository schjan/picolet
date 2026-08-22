---
status: accepted
---

# One Host per Linux user; picolet bootstraps whole Machines

picolet was built as one Agent per machine, keyed by `hostname`. Workloads that must not share a UID (e.g. a forge and the CI runner that executes foreign code) need separate Linux users, and Podman user namespaces, pods and sockets are per user — so each user needs its own Agent. We decided **not** to introduce a composite identity or a root daemon that deploys into user sessions. Instead a **Host** is simply "one Agent under one Linux user", an ordinary `hosts/<name>` entry, and a **Machine** is a box that runs several Hosts. `host.yml` declares `machine:` and `user:` (no `user:` = rootful); the name is a convention only (`<machine>`, `<machine>-system`, `<machine>-<user>`).

Because every Agent holds a full clone, every Linux user on a Machine can read the whole Fleet. This is accepted: git write access is the trust boundary for *desired state*, Linux users are the boundary for *runtime blast radius*, and nothing secret may ever be in git. Sparse or per-user checkouts were rejected as complexity without a threat model.

This reverses the earlier non-goal "no host provisioning": the many hand-written bootstrap procedures were the real pain, so `picolet bootstrap machine` (root, idempotent, re-runnable) creates users, verifies subuid/subgid, enables linger and the user podman socket, places per-Host config and credentials, and runs the per-Host bootstrap — deriving the whole plan from the Fleet. Package installation, firewall and sysctls stay outside picolet.

## Consequences

- No Go code parses Host names; metric labels gain `machine`.
- Default credential model is one read-only git token and one secret-provider token per Machine, placed into every Host's secrets directory by the bootstrap. Scoping provider tokens per Host (viewer role, only that Host's items) or giving a Host no provider at all is the documented stricter option for untrusted boundaries, not the default. Git tokens are never provider-resolved, so picolet stays repairable when the service it deploys is down.
- Cross-Host traffic on one Machine goes through published host ports or the public hostname, never through a shared Podman network.
