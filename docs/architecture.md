# Architecture

This page is how Chmura is put together underneath the model: the components, how
they talk, and the few deliberate choices — a control plane that lives outside the
cluster it manages, an agent that dials out, and an engine you never see.

## The pieces

```text
CLI / Web UI / SDK
        │  HTTPS
        ▼
Chmura API / control plane
        ├── state database
        ├── operations
        ├── revisions
        └── planners / controllers
                │  outbound agent connection
                ▼
        Chmura cluster agent
                │
                ▼
        cluster resources
```

The **CLI talks only to the API.** It never connects directly to servers, storage
pools, the orchestrator, or the cluster agent. Every other client — a future web
UI, an SDK — is the same: a client over the one control plane, never a second
source of logic.

## How they talk

The public protocol starts simple: HTTPS, JSON, a REST-like API, and
Server-Sent Events for progress and logs.

```text
POST /v1/spaces/{space}/projects/{project}/deployments
GET  /v1/operations/{operation}
GET  /v1/operations/{operation}/events
GET  /v1/projects/{project}/logs
```

A long-running request returns `202 Accepted` with an operation ID, and you watch
the operation rather than holding the connection open:

```json
{ "operation-id": "op_71f28a", "status": "pending" }
```

A future bidirectional protocol (WebSocket or similar) will back interactive
commands — `chmura app exec`, `chmura app shell`, `chmura port-forward` — but none
of that is needed for an ordinary deploy.

### The agent dials out

The cluster agent **initiates the connection**, outbound from the cluster to the
control plane. Nothing reaches *into* the cluster. That is what lets a cluster run
behind NAT, a firewall, or a home router, with no public address of its own.

The agent **reports facts; it does not declare configuration.** Locations, pools,
tags, and capabilities are declared by an administrator in
[`cluster.yaml`](reference/cluster.md); the agent confirms whether they can be
realized and reports the measured values. Losing the connection is not a signal to
stop anything — the agent keeps reconciling the last state it was given, buffers
events, and reconnects with backoff. The internal channel uses mTLS; the agent has
its own identity, separate from CLI users.

## Two planes

One CLI, one API, split only by permission:

- the **user plane** — spaces, projects, applications, volumes, endpoints — is
  everything in [Getting started](getting-started/first-steps.md) and the
  [domain model](concepts/domain-model.md);
- the **admin plane** — installations, clusters, locations, nodes, pools,
  policies — is [`cluster.yaml`](reference/cluster.md) and
  [`installation.yaml`](reference/installation.md).

The dividing line is the same taxonomy everywhere: **reported** facts (nodes,
capacity, health) come from the agent; **declared** configuration (locations,
pools, policies) comes from an administrator's files; **events** (join, drain,
deploy) are imperative commands. You cannot declare a node into existence, and you
cannot create a pool with a command.

## Bootstrap

The control plane **does not live in the cluster it manages.** That cuts the
chicken-and-egg problem: bringing up the control plane needs no working cluster.

```text
chmura-server   the control plane, API, and state database — its own artifact
chmura-agent    the cluster agent — its own artifact
chmura          the CLI — installs and starts neither of the above
```

On the control-plane host:

```bash
chmura-server init      # create the state DB and the first admin account
```

From there the installation exists, and the only way to talk to it is the API. A
node joins from **its own side** with a single-use token that carries its cluster
and location — the control plane never reaches out to install anything:

```bash
chmura-agent join --url https://chmura.company.example --token njt_...
```

Self-hosting the control plane on a cluster it manages is a future problem that
needs its own disaster-recovery story, and is not part of the first versions.

## Control-plane redundancy

`chmura-server` is **stateless**; all state is in the database. That reduces high
availability to a well-understood problem — a highly-available database — rather
than a bespoke consensus protocol.

```text
single mode   1 × chmura-server + an embedded database
HA mode       N × chmura-server + an external replicated database, any load balancer
```

The most important property falls out of the agent model:

!!! note "A control-plane outage is not a workload outage"
    Running applications keep running. The agent reconciles the last state it was
    assigned, restarts work, and buffers events. What you lose is the ability to
    *change* the system — new deploys, new operations, live status — not its
    *operation*. So a single control plane is a fine choice for a homelab; HA is
    an operational decision, not a condition of correctness.

The state database is the **only** thing that cannot be reconstructed from
elsewhere — agents know their assigned state but not identities, revision history,
or issued credentials. Backing it up is the administrator's basic duty; the key
that encrypts secrets lives outside it, so a backup is never a secret leak.

## The execution engine

An engine like k3s can run the workload underneath, but **its objects never
surface in the model** — that guarantee comes from the domain model, not from a
plugin system.

The engine is replaceable in code but **not a user choice** in the first versions.
One engine is supported; the `runtime.engine` field exists so that adding another
is not a breaking change. The engine is uniform within a cluster.

The rule that keeps the abstraction honest:

> Differences between engines surface only through declared capabilities and
> through degradation — never as fields in a project manifest.

If a second engine cannot do something, it shows up as a missing capability on a
pool, node, or cluster — the same vocabulary as `multi-attach` or a location — not
as a new key in `chmura.yaml`. From which follows a rule with no exceptions:
`chmura.yaml` never contains an escape hatch to the engine. No raw passthrough,
ever — one such hatch would void portability, and no one would notice until the
first migration.

## Remote operations

Every longer operation has a stable ID and a lifecycle you can observe or steer:

```bash
chmura deploy                       # watches the operation to completion
chmura deploy --detach              # returns the operation ID and exits
chmura operation inspect op_71f28a
chmura operation watch   op_71f28a
chmura operation cancel  op_71f28a
```

`Ctrl+C` while watching stops watching — it does **not** cancel the remote
operation, and it exits `130`, not `0` (see [exit codes](cli.md)). Cancelling
remotely is an explicit command.

Two guarantees make operations safe over an unreliable network:

- **Idempotency keys.** Every state-changing request carries one. If the
  connection drops after the request was accepted, retrying with the same key
  returns the existing operation instead of creating a second — for deploy,
  delete, scale, resize, rollback, move, and cleanup.
- **Revision conflicts.** A deploy carries the base revision it expected, read in
  the same command (never from a local file — [local state is never required for
  correctness](concepts/domain-model.md)). If someone else changed the project in
  between, the API returns a conflict instead of overwriting their change.

## Version and capability handshake

On connecting, the CLI asks the installation what it is:

```json
{
  "server-version": "0.4.2",
  "api-versions": ["v1"],
  "profile": "production",
  "features": ["volume-policies", "rolling-deployments", "server-sent-events"]
}
```

If a manifest needs a feature the server does not offer, the CLI or API says so
plainly rather than failing halfway. The `profile` (`production` or `dev`) is what
lets the CLI label a [dev installation](local-dev.md) everywhere a target is shown.

## Authentication

Users present a **token** over HTTPS, obtained by `chmura remote login` (stored in
the OS credential store, referenced — never written — in config) or supplied as
`CHMURA_TOKEN` in CI. The identity backend is replaceable: built-in accounts for a
homelab, or OIDC delegation for an organization, chosen in
[`installation.yaml`](reference/installation.md). The token carries *identity*;
what it may *do* is RBAC, which is deferred. The agent authenticates separately,
with its own mTLS identity.

## Terraform and space migration

**Terraform** is not Chmura's tool for managing applications. It can serve as an
adapter for the infrastructure underneath — machines, networks, DNS, addresses,
load balancers, empty volumes, provider resources — while Chmura owns desired
state, planning, revisions, rollout, verification, identity, and future
migrations. Terraform may execute a fragment of a plan; it is not the engine of a
space migration or of application data.

**Space migration** — moving a space to another cluster — is a future,
multi-step operation, not a single field change: inventory, preflight, capability
match, create target resources, copy data, test, verify, final sync, cut traffic,
observe, and clean up or keep the source. Because a space name is unique within an
installation, a migration never causes a name conflict, and the address
`remote:space` stays the same before and after — only the cluster underneath
changes. Full rollback after traffic has cut over, and an active multi-cluster
space, are explicitly out of the first generation.
