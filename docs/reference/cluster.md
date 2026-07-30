# `cluster.yaml` reference

`cluster.yaml` is the **declared** configuration of one cluster: its execution
engine, its locations, its ingress entry points, and its storage pools. It is
administrator-facing, applied with `chmura cluster apply`, and — like every
declared file — has no per-key create commands; you change the file and apply it.

```bash
chmura cluster apply company:home --file cluster.yaml --dry-run
chmura cluster apply company:home --file cluster.yaml
```

`cluster apply` is the admin-plane twin of `chmura deploy`: same plan, dry-run,
revisions, operation ID, idempotency key, and revision-conflict handling.

!!! note "Declared, not reported"
    `cluster.yaml` declares what an administrator provides. It does **not** list
    nodes — a node [joins from its own side](../architecture.md) with a token and
    is *reported*, not declared. The cluster itself is created first with
    `chmura cluster create`; the file fills it in.

All keys are kebab-case, the same convention as
[`chmura.yaml`](manifest.md#naming-conventions).

## Top level

| Key | Type | Required | Purpose |
| --- | --- | --- | --- |
| `version` | integer | yes | Schema version. Currently `1`. |
| `runtime` | object | yes | The execution engine. |
| `locations` | map | yes | Failure domains, keyed by name. |
| `ingress` | map | no | Named traffic entry points. |
| `storage-pools` | map | no | Storage pools, keyed by name. |

## `runtime`

```yaml
runtime:
  engine: k3s
```

| Key | Type | Purpose |
| --- | --- | --- |
| `engine` | enum | The execution engine, uniform across the cluster. One engine is supported in the first version; the field exists so adding another is not a breaking change. |

The engine is internal — its objects never surface in the user model, and engine
differences appear only as capabilities and degradation, never as manifest
fields. See [Architecture](../architecture.md).

## `locations`

```yaml
locations:
  server-room-a: {}
  server-room-b: {}
```

A location is a failure domain — a room, rack, zone, or site — that placement and
HA rules refer to (see [Domain model](../concepts/domain-model.md)). A node is
assigned to a location when it joins, and a pool declares which locations it
serves.

Location entries are currently empty maps; the key *is* the location. The map
form leaves room for future per-location metadata without a schema change.

## `ingress.<name>`

A named entry point for outside traffic. It is the uniqueness domain for
endpoints — see [Networking](../networking.md#ingress). A cluster with no ingress
accepts no external traffic: projects can deploy, but their endpoints have nowhere
to exist, which `chmura doctor` reports.

```yaml
ingress:
  default:
    addresses:
      - 203.0.113.10
  internal:
    addresses:
      - 10.0.0.5
    spaces:
      - production
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `addresses` | list | — | The addresses where traffic actually arrives. |
| `spaces` | list | all spaces | Optional restriction — only these spaces may use this ingress. Omitted means every space on the cluster. |

An endpoint selects an ingress by name, defaulting to `default`.

## `storage-pools.<name>`

A pool is a concrete storage resource the administrator provides. Pools are
**declared, never discovered**, and a project reaches them only through a
[storage policy](installation.md), never by name — which is what keeps projects
portable across clusters.

```yaml
storage-pools:
  main-ssd:
    backend:
      driver: zfs
      dataset: tank/chmura
    capacity: 2Ti
    locations: [server-room-a, server-room-b]
    capabilities: [multi-attach, snapshots, quota]
    tags: [ssd, ha, backup]
```

| Key | Type | Purpose |
| --- | --- | --- |
| `backend` | object | The storage driver and its driver-specific settings. |
| `capacity` | size | The limit the administrator grants Chmura — a declaration, not a measurement (see below). |
| `locations` | list | Which locations can use this pool. Must be a subset of the cluster's declared `locations`. |
| `capabilities` | list | Explicit technical capabilities (see below). |
| `tags` | list | Free-form administrator declarations, matched by storage policies. |

### `backend`

The `driver` names the storage technology; the remaining keys are
driver-specific.

```yaml
backend: { driver: zfs, dataset: tank/chmura }
backend: { driver: lvm, volume-group: nvme0 }
backend: { driver: dir, path: /srv/chmura }
```

The full set of drivers and their settings is deferred; the shape (a `driver`
plus its own keys) is fixed.

### `capacity` — declared, not measured

`capacity` is the limit an administrator *grants*, not what the disk *has*:

```text
capacity: 2Ti      declaration — how much Chmura may use
status.measured    measurement — how much the agent actually sees
status.allocated   measurement — how much is assigned to volumes
```

If the declaration exceeds what the agent sees, the pool is reported unsatisfied
rather than quietly trimmed:

```text
! Pool main-ssd declares 2Ti but the backing dataset provides 1.6Ti.
```

### `capabilities` — declared, never inferred

`capabilities` is an explicit list, for the same reason tags are: Chmura never
guesses that `nvme` means fast, or that ZFS means multi-attach. The capability
list is a **contract the volume model checks against**:

| Capability | What it lets a volume do |
| --- | --- |
| `multi-attach` | back a `shared` volume — many instances writing at once (RWX). |
| `resize` | back an elastic volume (omitted `size`) that grows on demand. |
| `quota` | enforce a volume's `max` as a hard cap at write time. |
| `snapshots` | point-in-time snapshots (for future backup/clone features). |

A volume whose needs no pool can satisfy is a plan error, before anything runs:

```text
Error: volume "uploads" needs allocation: shared,
       but no pool matching policy "balanced" declares capability "multi-attach".
```

Without this list, a `shared` volume, an elastic size, or a hard `max` could not
be validated ahead of time. These four are the **settled core** the model relies
on; the list is open to further capabilities later, but nothing today depends on
more than these. See [Storage](../storage.md) for how each is used.

### `tags`

Tags are arbitrary administrator declarations — `ssd`, `ha`, `backup`, `cheap`,
`encrypted` — that storage policies match on. Unlike capabilities, Chmura assigns
them no meaning; they mean whatever the administrator and the policies agree they
mean.

## Complete example

```yaml
version: 1

runtime:
  engine: k3s

locations:
  server-room-a: {}
  server-room-b: {}

ingress:
  default:
    addresses: [203.0.113.10]

storage-pools:
  main-ssd:
    backend: { driver: zfs, dataset: tank/chmura }
    capacity: 2Ti
    locations: [server-room-a, server-room-b]
    capabilities: [multi-attach, snapshots, quota]
    tags: [ssd, ha, backup]

  local-nvme:
    backend: { driver: lvm, volume-group: nvme0 }
    capacity: 1Ti
    locations: [server-room-a]
    capabilities: [snapshots, resize]
    tags: [nvme, fast]
```

Storage **policies** — the portable names a project asks for, like `fast` or
`balanced` — are not here. They are installation-wide, in
[`installation.yaml`](installation.md), so a policy means the same thing in every
cluster.
