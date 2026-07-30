# Local dev

The fastest way to see a project work is locally — and Chmura's local dev is not
a simulator. It is **the same `chmura-server` and the same execution engine**, run
in a **dev profile**: single-node, ephemeral, with a local image loop.

Tools like LocalStack have a flaw built in: they *reimplement* the service they
stand in for, and drift from it. "Works locally, fails remotely" is their natural
state. Chmura inverts that dependency — there is no separate dev engine, so there
is nothing to drift. This is the payoff of one engine of logic: because there is
exactly one implementation of a deploy, the local deploy **is** the deploy.

## `chmura-dev` is a separate tool

Standing an installation up and running the dev loop is different enough from
operating a running one that it belongs in its own tool, alongside the others:

```text
chmura         the CLI — talks to any installation's API
chmura-server  the control plane
chmura-agent   a node's agent
chmura-dev     the dev harness — stands up and drives a local installation
```

The boundary is sharp: `chmura-dev` **stands up** and drives (`init`, `up`,
`down`, and a watch loop); `chmura` **operates** a running installation. Once the
installation is up, `chmura-dev` gets out of the way and ordinary `chmura`
commands work against `local:dev` unchanged:

```bash
chmura-dev up
chmura status local:dev/
chmura logs   local:dev/
chmura app restart local:dev/hello/web
```

## It is just an installation at `local`

Local dev adds no new concept to the model — only an installation running in the
dev profile and a remote pointing at it. `chmura-dev up`, behind the scenes:

- starts a single-node installation (`chmura-server` + `chmura-agent`, dev
  profile) on this machine,
- registers the `local` remote and creates the `local:dev` space and project — an
  explicit bootstrap, not silent creation,
- builds the image locally and loads it straight into the node, skipping any
  registry,
- runs an ordinary deploy in the dev profile.

### Location does not matter

"Local" means the profile, not the place. A dev installation can run on this
machine, a dedicated dev box, a VM, or WSL — each is just a remote the CLI points
at. This is the same property as the whole address model: the CLI neither knows
nor needs to know where an installation physically runs.

### Deploying to `local` is allowed, and intended

`chmura deploy local:dev/` is not blocked — a manual redeploy is the same deploy,
just without the loop. Safety comes from labeling, not locking: the installation
reports its profile, and the CLI shows it wherever a target appears:

```text
Target:  local:dev   (profile: dev, ephemeral)
Project: hello
```

The profile is a label, not a lock — you deploy to dev deliberately, and never
mistake it for production.

### Reaching your app

`chmura-dev` forwards every named port to `localhost` and prints the address —
the same port-forward tunnel `chmura port-forward` provides, just automatic. You
do not declare an endpoint to reach an app in dev. Because it is a tunnel, the
local address is identical whether the installation runs here, in a VM, or in WSL.

## `chmura-dev init`

Like [`chmura init`](getting-started/install.md), `chmura-dev init` writes a file
and nothing more — `chmura.dev.yaml`. It detects the project's applications,
proposes `space: dev` and sensible watch paths, fills `values` and a `secrets`
skeleton with **references, never values**, and adds any secret-source files to
`.gitignore`. It stands nothing up and connects to nothing.

## `chmura.dev.yaml` is its own schema

This is **not** a manifest layer or an override of `chmura.yaml`. It is a separate
document with its own schema, read only by `chmura-dev`, never merged into desired
state. A separate schema — rather than extending the manifest — lets it describe
things the manifest has no business knowing (the dev space, value sources, the
loop) while guaranteeing it cannot change what the application *is*.

```yaml
# chmura.dev.yaml — its own schema, in Git
space: dev

values:                       # vars — non-sensitive
  region: local
  api-url: http://localhost:8080
  external-url: { from-env: DEV_EXTERNAL_URL }

secrets:                      # sensitive — always a source, never a value
  db-password: { from-env: DEV_DB_PASSWORD }
  api-key:     { from-file: ./dev/api-key }
  session-key: { generate: true }

build:
  api:
    watch:  [ ./api/src ]
    reload: /app/bin/reload
```

The file is in Git and **never holds a secret value** — the same reason
`chmura secret set` has no `--value`. A dev secret is always a reference to a
source.

## Where values come from

Seeding answers a question the model deferred — where `var` and `secret` values
come from outside a remote installation — without inventing new semantics. Values
still land in the space and still resolve project-then-space; the only difference
is that in dev a bootstrap seeds them from the repository.

Each value declares its source:

| Source | Meaning |
| --- | --- |
| literal in `values` | written inline (non-sensitive only) |
| `from-env: NAME` | a shell environment variable |
| `from-file: ./path` | a file (git-ignored) |
| `generate: true` | generated once per installation (local-only secrets) |

`generate` is for secrets no external party shares — the app is their only
consumer. The value is generated once, stored in the dev space, and consistent for
every application that references it; it disappears on `down` unless `--keep-state`.

### A missing source stops `up`

After seeding, `chmura-dev up` runs the same validation a deploy does. If the
manifest references a value no source can supply, the bootstrap **stops** and says
exactly what is missing and how to provide it — it never substitutes an empty or
random value:

```text
Error: dev value "db-password" (secret) has no source.

  declare one in chmura.dev.yaml:
    secrets:
      db-password: { from-env: DEV_DB_PASSWORD }

  or, for a local-only secret:
    secrets:
      db-password: { generate: true }

Nothing was started.
```

"What if not every shell variable is set?" gets the same answer as everywhere in
the model: fail fast with a named list, not a guess.

## Dev does not break "nothing is created automatically"

The rule that deploy creates no space or project holds in dev too. `chmura-dev up`
is not an exception — it is an **explicit bootstrap** that creates those objects
the way a person would with `space create` and `project create`. The difference is
convenience, not the rule:

```bash
chmura-dev up        # bootstrap: creates the installation, space, project, seeds values
chmura deploy        # still never creates — even against local:dev
```

## The dev profile

The differences between dev and production are **installation-level policies**,
reported by the handshake, never changes to the project. The manifest is untouched
and ships to staging without an "if dev" branch anywhere.

```text
production                        dev profile
──────────────────────────────   ──────────────────────────────
registry + pull to nodes         local build, loaded directly
certificate issuer (ACME)         self-signed cert, http on localhost
placement and HA rules            single node, spread rules ignored
instances.min enforced            floor relaxed to 1
durable state                     ephemeral, wiped on dev down
```

An app with `instances.min: 2` and an `exclusive` volume — which in production
needs a deliberate `floor` — simply comes up as one instance in dev, because
admission is the installation's call, not the project's. What dev **does not**
check is stated plainly (`chmura status local:dev/` shows the profile): HA,
location spread, real TLS termination, multi-node load. The list of differences is
closed, so "why does it work here but not there?" always has a finite answer.

A health check is not required in dev, either. An app with none runs
`process-only`, and if it does not come up you see it in the logs — the
stabilization window still catches a crash.

### Dev overrides

`chmura.dev.yaml` may set envelope-bounded dev values — an instance count, a
volume size — for example one replica instead of `preferred`:

```yaml
overrides:
  applications:
    api:
      instances: 1
```

This does not break the rule that the dev file cannot change desired state: these
are seeded as [overrides](deployment.md), not into `spec` — exactly what
`chmura app scale` does, declared in the bootstrap. The profile relaxation (floor
to 1) applies when no override is given; an override is for when dev should run a
deliberately different number.

## The dev loop

`chmura-dev` with no subcommand is the loop: bootstrap if needed, deploy, tail
logs, and react to source changes. The loop's rules — which paths to watch and how
to reload — are in `chmura.dev.yaml` per application.

```bash
chmura-dev            # up + deploy + watch + logs
chmura-dev down       # stops and wipes the dev installation
chmura-dev down --keep-state
```

### Two speeds, revisions by default

```text
source change, no reload   → rebuild image + redeploy → a REVISION
source change, reload set   → sync files + reload       → NO revision
```

By default the loop rebuilds and redeploys, creating revisions — this is closer to
production and a step from it, so it is the right default. Hot source-sync without
a revision is an **explicit opt-in** by declaring `reload` for an application.

Hot source-sync is the only place in the whole model where a running instance
changes without a new revision — which is why it is dev-only. It does not exist
against an ordinary remote; an instance under it is marked in status as "revision N
+ dev overlay," so no one mistakes it for reproducible state. This is the boundary
guarded most strictly: a revision stays immutable and reproducible everywhere
except an explicitly developmental, ephemeral installation.

## What dev does not change

Everything below is identical to production — and that is the point, not an
accident:

```text
address model         local:dev/hello/web means exactly what it always means
manifest              the same chmura.yaml, no "if dev" branch
value resolution      project-then-space, the same validation at deploy
health checks         the same mechanism
revisions             deploy and the default loop create revisions as usual
chmura commands        status, logs, app restart, scale — unchanged
```

If something works in `chmura-dev` but not after `chmura deploy` to staging, it is
a difference of installation profile — one node, no HA, self-signed TLS — not a
difference of engine. That list is closed and written above.
