# Observability

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

Status is a snapshot; observability adds the time axis. Signals split by who
produces them.

- **Events** — the domain timeline Chmura already produces (deploys, rollouts,
  restarts, degradations, node events), exposed addressably via `chmura events`.
- **Metrics** — platform metrics are always available; application metrics are
  opt-in, scraped from a named port. No declaration is stated plainly, never
  hidden.
- **Logs** — captured automatically from stdout/stderr, forwarded verbatim;
  Chmura does not scrub secrets.
- **Traces** — application-side; Chmura propagates context and forwards spans.
- **Storage** — our interface, a pluggable backend. A bounded built-in window;
  durable retention, dashboards, and alerting are an external sink wired in
  `installation.yaml`.
