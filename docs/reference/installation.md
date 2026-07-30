# `installation.yaml` reference

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

`installation.yaml` is the **declared** configuration shared across a whole
installation — the things that must mean the same in every cluster. It is applied
with `chmura installation apply` and, like [`cluster.yaml`](cluster.md), has no
per-key create commands.

Planned sections:

- **Storage policies** — the portable names a project asks for (`fast`,
  `balanced`, `cheap`), each a set of required/preferred/forbidden tags and
  fallbacks. Installation-wide so a policy means the same thing everywhere, which
  is what keeps a project portable when its space moves between clusters.
- **Certificate issuer** — the ACME directory, contact, and domain-control method
  behind `certificate: automatic`.
- **Observability** — the built-in retention window and the optional external
  `forward` sinks for metrics, logs, and traces.
- **Identity backend** — built-in accounts, or delegation to an external OIDC
  provider, behind `chmura remote login`.
- **Secrets backend** — the driver behind stored secrets, and where the
  encrypting key lives (never in the state database).
