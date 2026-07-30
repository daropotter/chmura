# Storage & volumes

A **volume** is a logical, durable resource with a stable identity of its own,
independent of the instance that uses it. You describe what a volume should be —
how it is shared, how big, how long it lives — and Chmura places it on a cluster
pool that satisfies your storage policy. The physical placement can change over
time; the logical volume ID does not.

## Shared or per-instance

A volume is one of two things, and that single choice also settles concurrency —
there is no separate "attachment" knob:

```text
shared         one volume, used by every instance at once (concurrent)
per-instance   one volume per slot, each used by its own instance (exclusive)
```

```yaml
volumes:
  uploads:                    # a shared upload directory, many writers
    allocation: shared

  data:                       # a database with a disk per replica
    allocation: per-instance
```

A **single database** — one volume, one writer — is just `per-instance` with one
instance. There is no third "shared but exclusive" mode: restricting a shared
volume to a single writer is really "run one instance," and that limit belongs in
`instances`, not here.

```yaml
volumes:
  db:
    allocation: per-instance   # with instances: 1 → one exclusive volume
```

`shared` is a requirement on the storage, not a wish: it can only land on a pool
that declares the `multi-attach` capability. If no pool satisfying the policy
declares it, the plan fails rather than quietly running a single instance. See
the pools in [Architecture](architecture.md).

## Mounting

A volume is defined at the project level, but each application mounts it
explicitly — it is not automatically visible to every app in the project:

```yaml
applications:
  api:
    mounts:
      uploads:
        volume: uploads
        path: /var/lib/uploads
```

## Sizing

Size is a range, like every other envelope:

```yaml
volumes:
  assets:
    size:
      min: 70Gi
      preferred: 100Gi
      max: 200Gi
```

- `min` — below this, the volume may not be created.
- `preferred` — the initial target size.
- `max` — the ceiling; it is the envelope for `chmura volume resize`, and growing
  past it needs a manifest change and a deploy.

If `preferred` will not fit, Chmura takes the largest value at least `min`,
rounds to the pool's allocation unit, and marks the volume `DEGRADED`. If not
even `min` fits, creation fails.

### `size` is optional — omitting it means elastic

Giving a `size` chooses a **bounded** envelope: a fixed range, no automatic
growth beyond `preferred`.

Omitting `size` chooses **elasticity**: the volume starts at the pool's smallest
allocation unit and grows on demand. This is the default where you would rather
not manage a size — and it is what `chmura init` leaves in place instead of
guessing.

```text
size given      bounded min/preferred/max, no auto-grow
size omitted    elastic: minimal unit + grow on demand
```

Elasticity depends on the pool. A pool that can grow — a directory-backed pool,
or one with the resize capability — supports it. A pool that cannot grow makes an
omitted size an **error** asking for an explicit one; Chmura does not silently
pick a fixed size. Auto-grow defaults are sensible and still being tuned (working
values: ~90% threshold, a step of at least 1 GiB, capped by the pool's capacity).

### Enforcing `max` on a directory-backed pool

`max` enforcement is a pool capability, not an assumption. Where the backend can
set a quota — a ZFS dataset, an XFS project quota, a btrfs qgroup — Chmura sets it
on the volume, and the kernel enforces `max` hard, at write time.

Where the backend cannot (a plain folder on a filesystem with no project quotas),
`max` **cannot** be a hard cap, and Chmura says so rather than pretending:

- usage is measured and reported, and a volume over `preferred`/`max` shows
  `DEGRADED`,
- new allocations against a pool over its `capacity` are refused (admission
  control at allocation time),
- but a single volume can still overrun `max` between measurements — polling
  `du` is reactive, not a write-time block.

The honest rule: a hard `max` needs a pool that declares the quota capability;
without it, `max` is advisory and monitored. This is the same
capability-over-assumption stance as elsewhere — a weak guarantee never looks
like a strong one.

Shrinking an existing volume is not supported in the first version — a smaller
size is an error that changes nothing.

### Per-instance size is always per instance

For `allocation: per-instance`, `min`/`preferred`/`max` describes **one slot**,
never the sum. The total requirement is `size × instance count`, and Chmura
checks that when placing. A combined reading would be a trap: scaling up would
split one pool across more instances, so adding a replica would degrade the space
of the ones already running.

## Slots, reclaim, and `reset`

A per-instance volume binds to a stable [slot](concepts/domain-model.md):

```text
api/0 → data/0
api/1 → data/1
api/2 → data/2
```

Across a restart or a deploy, the new runtime in the same slot recovers the same
volume — the slot is stable, so its binding is too. Scaling is a different
situation and has its own rules.

**Which slots leave.** Scale-down removes the **highest-numbered** slots first: 4
→ 2 drops `api/3` then `api/2`, leaving `api/0` and `api/1` untouched. This keeps
numbering compact and protects the lowest, canonical slots (`api/0` is often
special — a seed, the first writer). A vacated slot's volume is not destroyed
immediately; it detaches and enters its lifecycle (below).

**Reclaim on scale-up.** By default, numbering is compact: scale-up takes the
lowest actually-free number, so 2 → 4 reassigns `api/2` then `api/3` — the same
numbers scale-down released. Chmura *tries* to reattach the volume that belonged
to that number, but nothing by force:

```text
the slot's previous volume exists and is free   → reattached, data returns
it was removed by lifecycle                      → a fresh volume
the space was used in the meantime               → a fresh volume, possibly DEGRADED
```

!!! warning "Reclaim is best-effort"
    A per-instance volume's data may or may not survive scale-down and scale-up.
    It depends on the volume's lifecycle policy and whether the freed space was
    used in the meantime. Chmura does not hide this — promising durability that
    is not there would be worse than its absence.

### Slot numbering: `reuse` or `serial`

```yaml
applications:
  api:
    slots:
      numbering: serial     # default: reuse
```

```text
reuse (default)   compact numbers; a released number returns to the pool and is
                  reused (lowest free first); volume reclaim is best-effort
serial            numbers only increase and never return — like a serial counter;
                  volumes are not reclaimed
```

`serial` behaves like auto-increment: every instance ever created gets its own
number, never handed out again. Use it where reusing a slot number would be
misleading or unsafe — for example when the number has escaped as a durable
identity.

### `reset` — when a volume starts fresh

By default a per-instance volume is durable: the same slot recovers the same data
across restart and deploy. A workload with purely temporary state can ask for the
opposite — a fresh volume on every revision:

```yaml
volumes:
  scratch:
    allocation: per-instance
    reset: on-deploy       # default: never
```

```text
never (default)   durable; survives restart and deploy
on-deploy         each deploy starts from a new volume
```

With `reset: on-deploy` the old volume is not wiped in place — it detaches and
goes through cleanup, so protection and retention still apply. The `reset` axis
names *when* a volume is cleared, leaving room for future triggers without a flag
per case.

## Placement

Application placement and storage selection are resolved together:

- an instance can only run in a location where its volume is available,
- a shared volume for instances across several locations must be available in all
  of them,
- if both cannot be satisfied, the plan fails.

This is why an `exclusive` volume forces a `swap` rollout, and why per-instance
volume capacity gates scale-up — see [Deployment](deployment.md).

## Lifecycle and retention

### Stable identity

Every volume has an ID, a name, an owner, its bindings, a lifecycle, a
detachment time, and a state. Deleting a project must never let a new project of
the same name inherit the old volume.

The [identity model](concepts/domain-model.md) guarantees this: a volume's owner
is a `project-id`, not a name. A project recreated under the same name gets a new
ID, so old volumes stay `ORPHANED` until an explicit `chmura volume adopt`.

A volume's lifecycle is copied into the remote object at deploy time, so the
retention policy keeps applying after the project — and the `chmura.yaml` it came
from — is gone.

### Detached policies

When a volume detaches (a slot removed, a project deleted), what happens next is
its `detached` policy:

```yaml
lifecycle:
  detached:
    policy: retain            # never deleted automatically
```

```yaml
lifecycle:
  detached:
    policy: expire
    min-age: 30d               # deletable by the cleanup runner after min-age
```

```yaml
lifecycle:
  detached:
    policy: pressure
    min-age: 30d               # reclaimable after min-age, but deleted only when
                              # the pool needs the space
```

Under `pressure`, cleanup is strict FIFO: detached volumes only, past `min-age`
only, unprotected only, oldest detachment first, stopping as soon as enough
capacity is recovered.

Protection excludes a volume from all automatic cleanup:

```yaml
lifecycle:
  protect: true
```

### Orphaned volumes

A retained volume whose project was deleted becomes `ORPHANED`. You can inspect,
adopt, or delete it:

```bash
chmura volume list --space production --orphaned
chmura volume inspect vol_8f2a
chmura volume adopt vol_8f2a --project restored-shop
chmura volume delete vol_8f2a
```

## Policies and pools

A project never names a physical pool. It declares a portable **storage policy**
— a set of tag requirements — and Chmura matches it to a cluster pool that
satisfies them:

```yaml
volumes:
  uploads:
    storage:
      policy: fast
      tags:
        required: [ssd]
```

The same policy means the same thing in every cluster, which is what keeps a
project portable when a space moves between clusters. Pools, their capabilities,
and the policy vocabulary are declared by an administrator — see
[Architecture](architecture.md).
