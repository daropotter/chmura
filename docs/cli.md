# CLI contract

The command line is Chmura's primary interface. This page is the contract it
holds to: how it behaves, how you address things, and what the commands are.

## Principles

**Non-interactive by default.** A command never starts asking questions on its
own. If something is missing, it fails and tells you what is ambiguous, how to
supply it, and how to launch the wizard:

```text
Error: no application source was found.

Provide a source explicitly or launch the interactive configurator:

  chmura init --image ghcr.io/acme/app:1.0
  chmura init --interactive
```

**Interaction is explicit and reproducible.** A wizard runs only with
`--interactive` / `-i`. It is just a layer that collects data and feeds the same
engine as the non-interactive path — and it ends by showing the equivalent
command or the file it would write, so nothing it does is opaque. Secrets are
never written into a generated command; the wizard emits a reference instead.

**Confirmations are not questions.** A confirmation asks nothing new — it asks
whether you are aware of the reach of an effect you already described. It applies
to operations whose reach exceeds the named object or that cannot be undone
(deleting a project, space, volume, or value; restarting every app that uses a
changed value; anything acting on many objects at once):

1. the command first prints the full list of what it will touch,
2. at an interactive terminal it asks `[y/N]`, defaulting to no,
3. `--yes` (`-y`) satisfies the confirmation and skips the prompt,
4. without a terminal **and** without `--yes`, the command errors — it never
   waits.

Point 4 is the important one: it keeps CI from hanging on a prompt no one will
see.

**Layered help.** `chmura --help` shows only the mental model, the top commands,
the resource groups, and a few examples. Detail lives one level down
(`chmura node drain --help`). An unknown flag does not dump hundreds of lines:

```text
Error: unknown option "--replica"

Did you mean?
  --replicas

Run "chmura deploy --help" for available options.
```

**The stdout / stderr contract.** `stdout` carries the command's result and data
meant for further processing; `stderr` carries errors, diagnostics, interactive
messages, and verbose output. `--output json|yaml|table` is for machine-readable
results and is not a substitute for `--quiet`.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--dry-run` | Build a plan and validate; **never** change state. No `-n` shorthand. |
| `--quiet`, `-q` | No normal stdout; errors still go to stderr. Excludes `--verbose` and `--interactive`. |
| `--verbose`, `-v` | Show what Chmura is doing, step by step. |
| `--interactive`, `-i` | Launch the wizard. Without it, the CLI never prompts for data. |
| `--output`, `-o` | `table`, `json`, or `yaml`. |
| `--file`, `-f` | Use a manifest other than the detected `chmura.yaml`. |
| `-C` | Change into a project directory before running. |
| `--yes`, `-y` | Satisfy a required confirmation (see [Principles](#principles)). |
| `--fail-on-degraded` | Turn a `DEGRADED` result into a non-zero exit (see below). |
| `--detach` | Return the operation ID and stop watching; the remote operation continues. |

`--dry-run` guarantees no state change: it may read state, validate, plan, and
run safe preflight checks, but never writes desired state, creates a revision,
reserves resources, or makes "temporary" changes. If a safe dry-run is not
possible, the command refuses rather than guess.

### `--fail-on-degraded`

A rollout can succeed and still leave a project `DEGRADED` — fewer instances than
`preferred`, a volume smaller than `preferred`, a storage policy satisfied by
fallback. By default that is a **success**: degradation is an expected state
where you supplied minimums and fallbacks, not a failure. A pipeline that wants
to treat it as failure opts in with `--fail-on-degraded`, which changes only the
exit code — never what was deployed, and never triggering a rollback.

### Exit codes

```text
0    success, including a DEGRADED result without --fail-on-degraded
1    execution error — including a failed rollout, even after a successful rollback
2    bad arguments, configuration, or a missing confirmation
3    state conflict — a revision or project-identity conflict
4    a DEGRADED result under --fail-on-degraded
130  interrupted with Ctrl+C
```

`3` is separate from `1` because a conflict is the one error worth retrying after
refreshing state; everything else needs a change in the project. `4` is separate
because the deploy *succeeded* — a pipeline stopping on it does so on purpose and
must tell it apart from a failure. `Ctrl+C` while watching a rollout exits `130`
even though the remote operation keeps running; detaching cleanly is a separate,
deliberate path (`--detach`). Interruption means "I don't know how it ended," not
success.

## Addresses

This is the heart of the CLI. There is **one** address grammar:

```text
[remote:]space/project/application
```

An address is never read in isolation. Every command has a single **subject** —
a space, a project, or an application — and that subject decides what the last
segment attaches to. Segments are positional, filled from the right. So the same
number of segments means different things in different command groups, with no
ambiguity anywhere:

| Command group | Segments | Example | Means |
| --- | --- | --- | --- |
| `app` | 1 | `api` | application in the current project |
| | 2 | `shop/api` | project + application |
| | 3 | `production/shop/api` | space + project + application |
| project | 0 | *(none)* | the current project |
| | 1 | `shop` | project |
| | 2 | `production/shop` | space + project |
| `space` | 1 | `production` | space |

There is no "local vs remote address" split — there is one address that is
sometimes incomplete, and what is missing is filled from context: the current
directory, the project registry, and the remembered target.

Because application operations have their own `app` group, a project command
never needs the `project/application` form — which is what removes the collision
between `space/project` and `project/application`. It also fixes a rule for
future commands:

!!! note "One positional subject"
    A command has exactly one positional subject. If it should act at two levels,
    the wider level is a flag, not an alternative argument form:

    ```bash
    chmura volume list                      # volumes of the current project
    chmura volume list shop                 # volumes of project shop
    chmura volume list --space production   # volumes of the whole space
    ```

### The remote prefix

The remote name is a prefix ending in a colon:

```text
company:production/shop
```

Omitting it uses the remote chosen by the [selection order](#remote-selection).
The remote alone, without a space, is written with a trailing colon — useful for
installation-level commands:

```text
company:                       # e.g. chmura space list company:
```

### The space is always explicit

The space is never guessed or defaulted. In an address it is either written, or
it comes from the project's remembered target — never from a rule like "the first
available one." `production/shop` in a project command always means "project
`shop` in space `production`," even if a local project happens to be named
`production`.

A trailing `/` after the space means *use this space, and the project from the
current directory*:

```bash
chmura deploy company:production/
chmura status company:staging/
chmura logs production/
```

### Paths never compete with addresses

No command takes both an address and a path in the same position. A path appears
only where a command expects a path by definition, or as the value of an explicit
flag — so paths need no `./` prefix:

```bash
chmura init services/api            # a directory; init takes no address
chmura project link /home/user/shop # link takes a path; unlink takes an address
chmura deploy -C ../shop            # path as a flag value
```

## Remotes and targets

A **remote** is a named connection to one installation — an endpoint and an
identity, never a space. Manage them with:

```bash
chmura remote add company --endpoint https://chmura.company.example
chmura remote login company
chmura remote list
chmura remote inspect company
chmura remote remove company
```

### `chmura remote use`

One command sets either the default remote or the project's remembered target —
the argument's shape decides which:

```bash
chmura remote use company              # global default remote
chmura remote use company:production   # this project's target: remote + space
chmura remote current
chmura remote unset
```

The form with a space needs a project directory. The remembered target is local
project state (never in `chmura.yaml`), lets you run `chmura deploy` with no
address, and is shown before every state-changing operation:

```text
Target:  company:production
Project: shop
```

With no target and no address, a remote command errors — it does not fall back to
a default space.

<a id="remote-selection"></a>
### Remote selection order

For an address where the remote name is omitted:

1. a remote named in the address,
2. the project's remembered target,
3. the global default remote (`chmura remote use`),
4. the only configured remote,
5. otherwise, an ambiguity error.

The space has no equivalent list. It is given explicitly or comes from the
remembered target.

## `chmura init` and project files

`chmura init` inspects the local project and writes `chmura.yaml`. It does not
connect to a cluster, deploy, create a remote project, or require the API. It
writes the project's name explicitly, and adds `.chmura/` to `.gitignore` if that
file exists.

Detection reads **only declarative sources** — `Dockerfile` and `docker-compose`
— never the application's behavior. What it cannot derive (health checks,
endpoints, env/secret references, volumes) it writes as **commented scaffolding**
for you to verify, rather than guessed values. In a monorepo with several
Dockerfiles, `init` without `-i` does not guess which apps to include; the wizard
lets you pick and name them.

An existing manifest is not overwritten. `--update` fills in detected values
while keeping your configuration; `--force` regenerates from scratch.

### The files

| File | Role |
| --- | --- |
| `chmura.yaml` | The portable project definition. Belongs in Git. No space, remote, tokens, cluster, runtime IDs, or status. |
| `chmura.local.yaml` | Local override of `chmura.yaml`. Git-ignored. No space or remote — that is the remembered target. |
| `~/.config/chmura/config.yaml` | User config: remotes, the default remote, output and interactive settings. No project desired state. |
| `.chmura/state.yaml` | Local project state: the remembered target and the last-seen `project-id`. Git-ignored, never required for correctness. |

Precedence, low to high:

```text
1. built-in defaults
2. chmura.yaml
3. chmura.local.yaml
4. command-line flags
```

`chmura.dev.yaml` is **not** in this list — it has its own schema, read only by
[`chmura-dev`](local-dev.md), and cannot change desired state.

## Command reference

High-level shortcuts:

```bash
chmura init
chmura deploy
chmura status
chmura logs
chmura doctor
```

=== "Spaces & values"

    ```bash
    chmura space create   # always needs an explicit name and --cluster
    chmura space list / inspect / rename / delete

    chmura var set / get / list / delete
    chmura secret set / list / delete       # no `get` — write-only
    ```

    Values default to project scope; `--scope space` writes the shared level.
    `--restart-affected` on `set` rolls the apps that use a value.

=== "Projects & apps"

    ```bash
    chmura project create / rename / list / inspect / export / delete
    chmura project link / unlink            # local registry only

    chmura app list / inspect / logs / restart / scale
    chmura app scale --clear                # drop the override
    ```

    `project create` and `deploy --create` are the only ways a remote project is
    born; `project rename` is the only way its name changes. `restart` is an
    operation; `scale` is a choice inside the manifest's envelope, stored as an
    override.

=== "Volumes"

    ```bash
    chmura volume list / inspect / adopt / resize / delete
    chmura volume resize --clear

    chmura project overrides list / clear
    ```

=== "Admin plane"

    ```bash
    chmura installation inspect / apply
    chmura policy list / inspect
    chmura cluster create / list / inspect / apply / delete
    chmura node token create / list / inspect / move / drain / remove
    chmura storage pool list / inspect
    chmura ingress list / inspect
    ```

    There is no `chmura node add` — a node joins from its own side with a token.
    Pools, ingresses, and policies have no create/modify commands; they live in
    `cluster.yaml` and `installation.yaml` and change via `apply`. See
    [Architecture](architecture.md).

=== "Remotes & operations"

    ```bash
    chmura remote add / login / list / inspect / use / current / unset / remove
    chmura operation inspect / watch / cancel
    ```

=== "Observability & diagnostics"

    ```bash
    chmura logs      # --follow, --since, --tail, --output json
    chmura events    # --since, --type, --follow
    chmura metrics   # --output json
    chmura doctor
    chmura config explain / show
    chmura version
    chmura completion
    ```

    Logs, events, and metrics take an address that narrows scope to a space,
    project, or application. See [Observability](observability.md).

Local development runs through a separate tool, `chmura-dev`, not the CLI — see
[Local dev](local-dev.md). Ordinary `chmura` commands work against `local:dev`
unchanged.
