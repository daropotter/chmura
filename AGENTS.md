# AGENTS.md

Shared context for AI agents (and humans) working on **Chmura**. Keep it short;
detail lives in [`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`docs/development/`](docs/development/). This file records the invariants and the
way we work, so any agent starts with the same context.

Chmura is an open-source private-cloud platform with a CLI-first workflow. Read
[`docs/`](docs/) for the product model before changing behavior; start at
[`docs/index.md`](docs/index.md).

## Invariants (from the design docs — do not violate)

- **Go** for every artifact (`chmura`, `chmura-server`, `chmura-agent`, `chmura-dev`).
- The **CLI is the primary interface**; the API and a future web UI are clients
  over the same control plane, never a second source of logic.
- **`chmura.yaml` never contains an escape hatch to the engine.** Engine
  differences surface only as capabilities and degradation.
- **Kubernetes/k3s objects never leak into the user model.**
- Chmura-defined keys and enum values are **kebab-case**; environment variable
  names are `UPPER_SNAKE_CASE`.
- **Nothing is created silently** (spaces, projects, values) — a missing thing is
  a fast, precise error.
- **Exit codes:** `0` success · `1` execution error · `2` bad arguments,
  configuration, or a missing confirmation · `3` state conflict · `4` a
  `DEGRADED` result under `--fail-on-degraded` · `130` Ctrl+C.
- Every state-changing request carries an **idempotency key**.
- **Local state is never required for correctness.**

## How we work

- **Strict TDD, small steps.** Define the outcome, write failing tests, implement
  until green. One self-contained change per pull request.
- **Test the outcome, not the exact output format.** Assert the meaningful signal
  (a suggested name appears, the exit code), not full strings. Don't fight the
  framework (cobra/pflag) for cosmetics — adapt to its defaults; override only for
  semantic reasons (e.g. reserving `-v` for `--verbose`).
- **Never commit directly to `main`.** Branch (`<type>/<slug>`), open a PR; the PR
  description carries the meaning (goal, red-test evidence, implementation notes).
- **Conventional Commits** for every commit.
- **English** for everything in the repo (code, comments, docs), except the
  project name "Chmura".

Full workflow and PR description template: [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Where things are

- [`docs/`](docs/) — the product specification (the model, the CLI contract).
- [`docs/decisions.md`](docs/decisions.md) — settled **product/model** decisions.
- [`docs/development/`](docs/development/) — **engineering** stack, repo layout,
  and the engineering decision log.
