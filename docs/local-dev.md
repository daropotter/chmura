# Local dev

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

Local dev is **the same `chmura-server` and the same execution engine**, run in a
**dev profile** — single-node, ephemeral, with a local image loop. There is no
separate dev engine, so there is nothing to drift. This is the payoff of one
engine of logic: the local deploy *is* the deploy.

- **`chmura-dev`** — a separate tool that stands up and drives a local dev
  installation (`init`, `up`, `down`, and a watch loop). Ordinary `chmura`
  commands work against `local:dev` unchanged.
- **A dev profile** — a closed set of installation-level relaxations (local image
  load, self-signed TLS, single node, relaxed floor, ephemeral state). The
  manifest is never dev-specialized.
- **`chmura.dev.yaml`** — its own schema, read only by `chmura-dev`, seeding
  values from literals, `fromEnv`, `fromFile`, or `generate`; never a manifest
  layer.
- **The dev loop** — rebuild-and-redeploy (with revisions) by default; opt-in
  hot source-sync (`reload`) is the only place an instance changes without a
  revision.
