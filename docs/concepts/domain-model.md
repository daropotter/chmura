# Domain model

Chmura's model is small on purpose. A dozen concepts cover everything from a
single home server to a multi-location cluster. This page introduces them and
shows how they nest.

Two of them — **remote** and **space** — are how you address *where* something
runs. Everything else is a resource that lives *inside* an installation.

## Installation

A single running Chmura: its public API, control plane, state database,
controllers, authentication, and the registry of clusters, spaces, and projects.

An installation is the unit of authority. When you address a space, exactly one
installation owns it.

## Remote

A **remote** is a named, local connection to one installation. It answers a
single question:

> Which installation am I talking to, and as whom?

```yaml
remotes:
  home:
    endpoint: https://chmura.home
    credential: home-account
  company:
    endpoint: https://chmura.company.example
    credential: company-account
```

A remote does **not** contain a space. The space is part of a resource's
address, not part of the connection. Tokens and passwords are never stored in
plain YAML — the config holds only a reference to a secure store.

You can mark one remote as your default (`chmura remote use company`), but that
is a local convenience. It never changes the meaning of an address where the
remote is given explicitly.

## Cluster

A **cluster** is a pool of execution resources: compute, storage, network,
locations, and technical capabilities. One cluster can back many spaces.

Clusters are created explicitly and start empty — a name, no nodes, no storage
pools. A node joins a cluster from its own side (see
[Architecture](../architecture.md)); the control plane never reaches out to
install anything on a machine.

## Location

A **location** is a failure domain inside a cluster: a server room, a rack, a
building, a network zone, or a geographic site. Placement and high-availability
rules are expressed in terms of locations, never individual machines.

## Space

A **space** is a logical area that holds projects and their resources. It
belongs to an installation and is bound, at any moment, to exactly one cluster.
That binding can change (a migration); the space's identity does not.

A space can hold projects, logical volumes, endpoints, secrets and config
values, logical networks, and bindings to the cluster's physical resources.

Two rules matter most:

- **Space names are unique within an installation**, not per cluster. That is
  what makes the address `remote:space` unambiguous without naming a cluster.
- **Nothing is created automatically.** There is no built-in `default` space. A
  space is created explicitly, with a name and a cluster:

    ```bash
    chmura space create company:production --cluster home
    ```

    Deploying to a space that does not exist is an error, not a trigger to
    create it.

A space is never a local concept — it does not appear in `chmura.yaml`. It shows
up locally only as part of a project's remembered target.

## Project

A **project** is one whole system, described by a single manifest. It can
contain several applications.

A project belongs to exactly one space. The same working directory can map to
different remote projects in different spaces — those are separate projects with
separate identities, not one project in two places.

Identity is a stable, server-assigned `project-id`. The **name** is the
recognition key within a space: a project is found by the pair `(space, name)`,
and the name must be written explicitly in `chmura.yaml`. This is what lets a
fresh clone reach the right project with no local state at all — the name is in
Git, the space is in the address.

## Application

An **application** is a single runnable component of a project — a frontend, an
API, a worker, a scheduler, a game server, a database run as part of the
project.

Application names are always explicit in the manifest. There is no hidden
"default application", and application operations always take an application
name, even when the project has only one.

## Instance slot and runtime instance

An **instance slot** is a stable position for an application instance:

```text
api/0
api/1
api/2
```

A slot stays stable across restarts, deploys of a new revision, and runtime
swaps. It can own its own `per-instance` volume.

A **runtime instance** is the concrete, momentary process or container behind a
slot. When the image is swapped, the slot stays and the runtime changes:

```text
Slot:  api/0          stays
Runtime ID:           changes
Revision:             changes
Volume binding:       stays
```

## Storage: pools, policies, volumes, bindings

Storage separates *what an administrator provides* from *what a project asks
for*, so that projects stay portable across clusters.

- A **storage pool** is a concrete storage resource in a cluster, declared by an
  administrator: a name, capacity, locations, capabilities, and tags.
- A **storage policy** is a portable set of requirements. A project names a
  policy and Chmura picks a matching pool. `fast` means the same thing in every
  cluster — which is why moving a space between clusters preserves its
  guarantees.
- A **volume** is a logical, durable resource with a stable ID, an owner, a
  required and allocated size, a policy, allocation and attachment modes, a
  lifecycle, and a physical binding.
- A **binding** is a volume's current physical realization. It can change during
  a future migration, but the logical volume ID stays.

## How it all nests

```text
Installation
├── Cluster
│   ├── Location
│   ├── Node
│   └── Storage Pool
└── Space                       ── bound to exactly one Cluster at a time
    ├── Project
    │   ├── Application
    │   │   ├── Instance Slot
    │   │   └── Runtime Instance
    │   ├── Volume
    │   ├── Endpoint
    │   └── Secret / Config value   (project scope)
    ├── Secret / Config value       (shared, space scope)
    └── Logical network resources
```

A space is a child of the installation, not of a cluster. A cluster backs a
space through a binding:

```text
Space production ──binding──> Cluster company
```

A remote is not part of this hierarchy at all — it is local access
configuration. An address ties the two together:

```text
remote : space / project / application
  │        └────────── remote hierarchy ──────────┘
  └── local connection configuration
```

The full grammar of addresses — remote, space, project, application, and how
much you can omit in each context — lives in the
[CLI contract](../cli.md#addresses).
