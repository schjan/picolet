---
status: accepted
---

# Quadlet is the config

picolet does not have a container or service model of its own. The Fleet repository contains raw Quadlet and systemd unit files (optionally as Go templates), and picolet's job is to select them per Host, render them, validate them with Podman's own `quadlet.Convert*` and reconcile them onto disk. A compact, Compose-like model that compiles to Quadlet was considered and rejected: it would have to chase Podman's feature surface forever, while passthrough gives every new Podman feature to the Fleet for free.

## Consequences

- A new Quadlet or systemd unit type must cost picolet at most a table entry (extension → destination and apply order), never a schema change.
- Pressure to change Go for a new service is a smell in the *file-selection or templating* layer, not a reason to add a model. An audit of the first 127 commits found exactly one missing passthrough (`.pod`) and zero service-specific code after the initial import.
- Anything Quadlet cannot express (ordering semantics, health gating, re-running one-shots) is solved with systemd/Podman features in the unit file or with Hooks, not with picolet-side abstractions.
