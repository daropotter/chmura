# Architecture

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

- **Communication** — the CLI talks only to the API; the agent connects outbound
  from the cluster; HTTPS + JSON + SSE; long operations return an operation ID.
- **The admin plane** — installations, clusters, locations, nodes, storage pools,
  and policies; what is reported by the agent vs declared by an administrator.
- **Bootstrap** — `chmura-server init`; the control plane does not live in the
  cluster it manages; nodes join with a single-use token.
- **Control-plane redundancy** — a stateless server over a highly-available
  state database; a control-plane outage is not a workload outage.
- **The execution engine** — internal and replaceable, not a user choice; engine
  differences surface only as capabilities and degradation, never as manifest
  fields.
- **Remote operations** — dry-run, operation IDs, idempotency keys, revision
  conflicts, and the version/capability handshake.
- **Terraform and space migration** — Terraform as an infrastructure adapter, and
  space migration as a future multi-step operation.
