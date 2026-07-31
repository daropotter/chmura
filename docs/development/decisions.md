# Engineering decisions

Settled decisions about **how Chmura is built** — the stack, the workflow, the
toolchain. Each entry records the decision, the reasoning, and any consequences,
so the *why* survives.

This is distinct from [`docs/decisions.md`](../decisions.md), which records
**product and model** decisions for users. This log is for contributors.

---

## Base language: Go

Every artifact (`chmura`, `chmura-server`, `chmura-agent`, `chmura-dev`) is
written in Go.

**Why.** The architecture calls for self-contained static binaries — Go's native
territory. The execution engine is k3s, and the whole control ecosystem
(`client-go`, `controller-runtime`) is in Go; the agent is a reconciler, which is
literally the controller-runtime pattern. The agent dials out over a long-lived
mTLS connection — idiomatic Go with gRPC streaming. The CLI contract (layered
help, "did you mean", command groups, config precedence) maps onto cobra + viper.

**Consequences.** One language across all artifacts, with no CLI-vs-API split.
Rust was rejected (slower iteration, weaker k8s ecosystem); Python/PHP/C# for the
single-binary distribution and the reconciler.

## Monorepo

One repository, one `go.mod`, binaries as `cmd/<artifact>`.

**Why.** The four artifacts share types, contracts, and logic (the model, the
API, PKI). A monorepo gives them one versioning story, one pipeline, and no drift
between binaries. See the [repo layout](layout.md).

## Agent ↔ control-plane channel: gRPC (+ mTLS)

The internal agent-to-control-plane channel is gRPC (streaming) over mTLS. The
public API is unchanged: HTTPS + JSON + SSE.

**Why.** A strongly typed contract fits Go; streaming suits reconcile and events;
mTLS gives the agent its own identity. WebSocket remains open only for the future
interactive channel (`app exec`, `port-forward`) — a separate decision.

## Working method: strict TDD, small steps

Outcome → failing tests → implementation until green → review on the PR → merge →
next small step.

**Why.** Controlled increments; every step is verifiable and reversible. The full
workflow is in the repository's `CONTRIBUTING.md`.

## Conventional Commits

All commit messages and PR titles follow Conventional Commits (`feat:`, `fix:`,
`docs:`, `refactor:`, `test:`, `build:`, `chore:` …).

**Why.** A consistent history, ready for automated changelogs and versioning.

## English for everything in the repo

Everything committed — code, comments, docs, config — is written in English. The
one exception is the project name, "Chmura".

**Why.** The repository is public; a single language lowers the barrier for
contributors.

## Work through pull requests, never directly on `main`

No commits straight to `main`. Every self-contained change goes through a branch
and a PR. Branch names are low-significance (`<type>/<slug>`); the weight is in
the **PR description** (goal, red-test evidence, implementation and difficulties).
The default branch is `main`. A PR merges (squash) after review and green CI.

**Why.** `main` stays green and reviewed; the history is legible through PRs; the
PR description becomes durable "why" documentation for each change.

## Test outcomes; don't fight the framework for cosmetics

Tests assert the intended **outcome** (e.g. "the user is shown they mistyped a
command or flag"), not the byte-exact format. We adapt to cobra/pflag defaults
rather than restyling their output; overrides are only for **semantic** reasons
(e.g. reserving `-v` for `--verbose`, not `--version`).

**Why.** Over-specified tests are brittle and lock in incidental format; fighting
the framework adds code with no user value. The goal is the user noticing the
error, not pixel-perfect output.

**Consequence.** cobra already suggests close commands natively, so a stage that
only reformatted that suggestion was cosmetic and was dropped. Flag suggestions
stay, because pflag offers none — that is real value, not restyling.

## Go floor: 1.23 → 1.25

`go.mod` declares `go 1.25` (development on 1.25.5).

**Why.** Greenfield with no users, so raising the floor costs ~nothing. CI reads
`go-version-file: go.mod`, so the floor *is* the CI version — at 1.23 CI quietly
tested on 1.23 while development was on 1.25.5; this aligns them (no `ci.yml`
change). It also unlocks `t.Chdir` (1.24) and `testing/synctest` (1.25); the
latter will matter for testing the concurrent agent, reconcile loops, and control
plane.
