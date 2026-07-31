# Contributing to Chmura

Chmura is in early implementation. This guide describes how we build it, so
changes stay small, tested, and consistent. For the product model, read
[`docs/`](docs/); for the engineering picture, read
[`docs/development/`](docs/development/).

## Prerequisites

- **Go 1.25+** (the module floor is `go 1.25`; CI uses the version in `go.mod`).

Common commands:

```bash
make test        # go test ./...
make vet         # go vet ./...
make build       # build all four binaries into bin/ (with version injected)
```

## The loop: strict TDD, small steps

Every change is a small, self-contained increment. No shortcuts:

1. **Outcome** — decide what should observably work.
2. **Tests first** — write them; they must fail. Keep the failing output (it is
   evidence for the PR).
3. **Implementation** — add code until the tests are green.
4. **Pull request** — one increment per PR; the review happens on the PR.

Don't merge stages together, don't write implementation before the test, and
don't outrun the scope of the change.

## Testing philosophy

- **Test the outcome, not the implementation.** Assert the meaningful signal — a
  suggested name appears, the exit code is right — not the exact wording or layout
  of a message. Over-specified tests are brittle and lock in incidental format.
- **Don't fight the framework for cosmetics.** cobra and pflag already do a lot;
  adapt to their defaults rather than restyling their output. Override only for
  **semantic** reasons (for example, reserving `-v` for a future `--verbose`
  instead of letting it mean `--version`).

## Pull requests

- **Never commit to `main`.** Branch first. Branch names are low-significance
  (`<type>/<slug>`, e.g. `feat/cli-exit-codes`); the meaning lives in the PR
  description.
- **Conventional Commits** for commit messages and PR titles
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `build:`, `chore:` …).
- **English** everywhere in the repo, except the project name "Chmura".
- CI (`go vet` + `go test -race`) must be green before merge.

### PR description template

```markdown
## Goal

<Which change this is and the outcome it targets — the observable behaviour that
should work after merge.>

## Red tests (evidence)

The failing run before implementation (proof the tests came first):

​```text
<paste the failing `go test ./...` output>
​```

## Implementation & difficulties

<How it was built, and any obstacle worth remembering — a framework quirk, a
contract conflict, a design trade-off. Be specific.>
```

We don't paste green test output — CI is the proof. The "difficulties" section is
institutional memory: the traps you hit help the next contributor avoid them.

## Style

- Match the surrounding code — its naming, comment density, and idioms.
- Chmura-defined keys and enum values are **kebab-case**; environment variable
  names are `UPPER_SNAKE_CASE`.
- Keep `cmd/<artifact>/main.go` thin; put logic under `internal/`.

See [`docs/development/layout.md`](docs/development/layout.md) for the repo layout
and [`docs/development/stack.md`](docs/development/stack.md) for the stack.
