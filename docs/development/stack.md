# Technical stack

The guiding principle is **don't reinvent the wheel**: Chmura is a thin,
opinionated layer over k3s, assembled from proven libraries. The library choices
below are candidates to confirm in code as each subsystem lands. The rationale
for the settled choices is in the [engineering decisions](decisions.md).

## Language and runtime

- **Go** — module floor `go 1.25` (development on 1.25.5). CI reads
  `go-version-file: go.mod`, so the floor is also the CI version.

## Artifacts (separate binaries)

- `chmura` — the CLI (the primary interface).
- `chmura-server` — control plane, API, and state database. Stateless (all state
  in the DB).
- `chmura-agent` — the cluster agent; dials out, mTLS, a reconciler.
- `chmura-dev` — the local development profile (a separate tool; ordinary
  `chmura` commands work against `local:dev` unchanged).

## Libraries (candidates — to confirm in code)

| Area | Candidate | Note |
|---|---|---|
| CLI + layered help | `spf13/cobra` | command groups, "did you mean", exit codes |
| Config + precedence | `spf13/viper` | defaults → `chmura.yaml` → `chmura.local.yaml` → flags |
| HTTP/JSON/SSE API | `go-chi/chi` + stdlib `net/http` | SSE is trivial |
| State DB (single mode) | SQLite (embedded) | "single mode" |
| State DB (HA mode) | PostgreSQL (external, replicated) | "HA mode", any load balancer |
| DB layer | `sqlc` | typed queries; must serve both engines |
| Agent ↔ control plane | gRPC (bidi stream) + mTLS | outbound; the agent has its own identity |
| k3s engine | `k8s.io/client-go`, `sigs.k8s.io/controller-runtime` | reconcile; k8s objects never leak into the model |
| YAML manifests | `goccy/go-yaml` | `chmura.yaml`, `cluster.yaml`, `installation.yaml`; schema validation |
| Credential store | `zalando/go-keyring` | token in the OS store, only a reference in config |
| PKI / mTLS | stdlib `crypto/x509`, `crypto/tls` | reach for step-ca/cfssl only if hand-rolling gets heavy |
| Interactivity (optional) | `charmbracelet/gum` (subprocess) | never business logic |

## Public contracts (from the docs — do not break)

- Public API: **HTTPS + JSON + REST-like + SSE**; `202 Accepted` + an operation
  id for long operations.
- Agent channel: **mTLS**, a separate identity, outbound, reconciling even when
  the connection is lost.
- **`chmura.yaml` never contains an escape hatch to the engine.** Engine
  differences surface only as capabilities and degradation.
- Chmura keys and enum values are **kebab-case**; `UPPER_SNAKE_CASE` only for
  environment variables.
- Every state-changing request carries an idempotency key; a deploy carries the
  base revision it expected.

## Open questions

- WebSocket vs gRPC for the future interactive channel (`app exec`,
  `port-forward`).
- Which encrypting backend for secrets on day one (the key never lives in the
  state database).
