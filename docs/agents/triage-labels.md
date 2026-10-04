# Triage labels

Canonical roles map directly to GitHub labels in this repository.

| Role | GitHub label | Meaning |
| --- | --- | --- |
| bug | `bug` | Existing behavior is broken |
| enhancement | `enhancement` | New feature or improvement |
| needs-triage | `needs-triage` | Maintainer evaluation or a design decision remains |
| needs-info | `needs-info` | Waiting for information from the reporter |
| ready-for-agent | `ready-for-agent` | Fully specified for an AFK agent |
| ready-for-human | `ready-for-human` | Requires human implementation |
| wontfix | `wontfix` | Will not be actioned as a separate request |

Every triaged issue carries exactly one category role (`bug` or `enhancement`) and one state role (the remaining five). Preserve unrelated labels such as `documentation` and `duplicate`. If existing state roles conflict, ask the maintainer before changing them.

An unlabeled issue normally enters `needs-triage` before a final disposition; the maintainer may approve a direct transition. Attach an agent brief when moving to `ready-for-agent`. A superseded issue may carry `duplicate` alongside its category and `wontfix` state, with a comment linking the issue that retains the work.

Edit this mapping if the repository adopts different label names.
