# Deployment & rollout

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

- **Compute and scaling envelopes** — `min`/`preferred`/`max` for CPU, memory,
  and instances, and what degradation means.
- **Health** — one `check` mechanism, two rules (`ready`, `restart`), and why
  restart depends only on the process.
- **Volumes** — allocation (`shared`/`per-instance`), attachment
  (`exclusive`/`concurrent`), sizing, lifecycle, slot binding, reclaim, and
  `reset`.
- **Replacement modes** — `surge` vs `stop-first`, declared not inferred; the
  `floor`; rollout capacity arithmetic and fail-fast placement.
- **Readiness, stabilization, and rollback** — what the observation window
  watches, and why rollback restores a revision but never data.
- **Local manifest vs remote state** — `metadata`, `spec`, `overrides`,
  `status`, revisions, and the imperative-vs-declarative envelope model.
