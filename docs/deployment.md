# Deployment & rollout

`chmura deploy` ships a project's desired state and rolls it out safely. This
page covers what "safely" means: the envelopes you declare, how Chmura decides an
instance is healthy, how a new revision replaces the old one, and how the local
manifest relates to the state the server keeps.

## Resource and scaling envelopes

Compute and instance counts are declared as a range, not a number:

```yaml
applications:
  api:
    resources:
      cpu:
        min: 100m
        preferred: 500m
        max: 2000m
      memory:
        min: 128Mi
        preferred: 512Mi
        max: 1Gi
    instances:
      min: 2
      preferred: 4
      max: 10
```

- `min` — below this, the instance cannot start (resources) or the project is
  unsatisfied (instances).
- `preferred` — the nominal value Chmura aims for.
- `max` — the ceiling for vertical scaling or autoscaling.

A scalar is shorthand for a fixed range — `memory: 512Mi` means
`min = preferred = max = 512Mi`.

An instance given at least `min` but less than `preferred` runs **degraded**, and
status shows all of it — required minimum, preferred, allocated, max, and the
reason. Degradation is an expected state where you declared a range, not a
failure.

!!! note "One envelope, three jobs"
    `min`/`preferred`/`max` is the same declaration read three ways: the
    **degradation** envelope (run smaller if you must), the **scaling** envelope
    for `chmura app scale` and autoscaling, and — with `floor` — the **rollout**
    envelope. Three questions, one declaration.

### Scaling and storage capacity

A new instance with an `exclusive` volume needs room for its own volume, so
scaling up follows the same rule as creating one — and never forces it:

```text
fits preferred          instance starts, volume at preferred
fits at least min       instance starts, volume DEGRADED, warning
does not fit min         scaling that instance fails
```

A scale-up that cannot place a volume at `min` stops at that instance rather than
squeezing it in somewhere. See [Storage](storage.md) for volume placement.

## Health checks

**Readiness and health are two different questions.** Conflating them is a
classic cause of outages, so Chmura keeps them apart and asks you to answer each
one consciously.

- **Ready** — *can this instance serve a request right now?* It has started,
  configured itself, connected to what it needs, and passed whatever checks it
  runs. A readiness check often reaches beyond the process itself — it may confirm
  a live connection to a dependency.
- **Healthy** — *is the process itself still working?* Sometimes "is it alive" is
  enough; sometimes the process is up but wedged — hung, disconnected, its session
  expired. A downstream blip should usually **not** make an application unhealthy.

Readiness governs **traffic**; health governs **restart**. They are two rules:

| Rule | Asks | Consequence | May depend on a dependency? |
| --- | --- | --- | --- |
| `ready` | Can it serve? | in or out of traffic | yes — that is often the point |
| `healthy` | Is the process itself working? | kill and restart in the same slot | **no — the process only** |

```yaml
applications:
  api:
    health:
      check:                    # the readiness probe — may reach a dependency
        http: { path: /ready, port: http }
        interval: 5s
        timeout: 2s
      ready:
        unready-failures: 3
      healthy:                  # opt-in; looks only at the process by default
        unhealthy-failures: 6
      startup-timeout: 2m
```

`health.check` is the **readiness** probe. `healthy` is the **health** rule, and
it never observes the readiness check — by default it watches only the process,
and you give it its own `check` to look deeper (below). This is the lesson of a
real incident, built into the model: a readiness check may legitimately test a
downstream, but the thing that *kills* an instance must look only at the process,
or one overloaded dependency takes the whole fleet down at once. The load
balancer's [panic threshold](networking.md#never-route-to-zero) guards the other
side of the same failure.

### Check types

```yaml
http:    { path: /healthz, port: http, expect-status: [200] }
tcp:     { port: signaling }
process: { name: myapp }                  # a named process/executable is running
exec:    { command: ["/bin/check-queue"] }
```

`http` for HTTP services, `tcp` for services that do not speak HTTP (databases,
TURN, binary protocols), `process` to confirm a named process or executable is
running, `exec` for everything else including port-less workers (exit `0` means
healthy). An `http`/`tcp` check names a **port by name**, never a number. `exec`
is the general extension point: whatever an app considers "healthy" is a script,
not a new manifest field.

The container's own exit is watched with no check at all — a crash is always
restarted in the same slot (the classic "stay alive"). The `process` check and
the `healthy` rule sit *above* that, catching a process that is running but
wedged.

### `ready`

```text
successes          successes in a row that grant readiness   default 1
unready-failures   failures in a row that revoke it          default 3
```

Readiness drives both traffic and rollout progress. The defaults suit a typical
HTTP service, so a declaration is often just `path` and `port`.

### The `healthy` rule is opt-in

Omitting `healthy` means **nothing ever kills a running instance except its own
exit**. That is not a gap: a process crash is observed always, with no check at
all, and is restarted in the same slot. The `healthy` rule exists only to catch
*hangs* — a process that is alive but no longer working. Most apps never need it,
and misused it turns a blip into a restart storm, so it is opt-in.

By default `healthy` watches only the process. To probe deeper — to catch a wedged
process that is technically alive — give it its own check, and keep that check
local so it cannot fail on a downstream:

```yaml
healthy:
  unhealthy-failures: 6
  check:
    exec: { command: ["/bin/self-check"] }   # the process, not a dependency
```

`healthy` does not apply until `ready` has succeeded once; if readiness never
arrives within `startup-timeout`, the instance is considered failed. That removes
the need for a separate startup probe.

!!! warning "Never kill on a shared dependency"
    The classic mistake is pointing the killer at a dependency: the database
    blips and *every* instance is killed at once — a restart storm exactly when
    the system is already struggling. In Chmura this is off by default (`healthy`
    never uses the readiness check), and when you do give `healthy` a check, it
    must look at the process, not a downstream.

### No check is allowed, but never invisible

Many workers have no meaningful notion of readiness, and forcing a fake endpoint
would be worse than none. Without a check, readiness means "the process started
and did not exit during the stabilization window." That mode has a name and
appears wherever state is shown:

```text
Application worker
  readiness: process-only
```

A public endpoint over an app with no check is worth a warning, and
`chmura deploy --dry-run` and `chmura doctor` say so plainly — a weak guarantee
never looks like a strong one.

### Readiness and traffic

Readiness controls traffic membership, not just rollout progress:

```text
ready         receives traffic from endpoints and project/space-visible ports
not ready     taken out of traffic, but not killed
restart fired killed and restarted in the same slot
```

Distinguishing lost-readiness from a restart is what lets an app survive a blip:
it stops receiving traffic but keeps its in-memory state and its volume. This is
the same readiness the [edge proxy and load balancer](networking.md) use.

## Rollout strategies

### The replacement mode is declared

Replacing instances with a new revision happens in one of two modes:

```text
surge   a new instance starts and becomes ready before the old one stops;
        the ready count never drops
swap    the old instance stops before the new one starts;
        the ready count drops by the batch size
```

```yaml
deploy:
  strategy:
    replace: surge     # the default
```

The mode is **declared, not inferred**. Chmura does not pick it from your volumes
or anything else — it checks whether the declared mode is feasible and refuses
the plan if not. `surge` is the default because it alone takes nothing away.
Where it is infeasible, the plan says so and requires an explicit `swap`:
lowering the ready count during a rollout should be visible in the manifest and
in review, not inferred silently.

### Volumes and swap

`replace` interacts with volumes, and the rule follows the **data**, not a rigid
"exclusive means swap":

- **`shared` volume** — concurrent by nature, so surge is always fine.
- **Durable `exclusive` volume (the default)** — the new runtime for a slot
  needs *that slot's data*, so it must take over the same volume, which the old
  runtime holds. That is exactly what `swap` does: same slot, volume handed over
  in place. Surge cannot preserve the data, so this requires `swap`.
- **Ephemeral `exclusive` volume (`reset: on-deploy`)** — the volume starts
  fresh each deploy anyway, so surge is fine; the surged instance simply gets its
  own fresh volume in a new slot.

So it is not that an exclusive volume forbids surge — it is that carrying data
across a rollout needs the old holder to let go first. There is no separate
`handover` field; this is entirely `replace` plus the volume's durability. See
[Storage](storage.md).

### Batches

```yaml
batch:
  size: 1          # a fixed count
# or
  percentage: 10   # rounded up, minimum one
# or
  partitions: 3    # split into roughly equal batches
```

`size`, `percentage`, and `partitions` are mutually exclusive. The default
strategy is `replace: surge`, `batch.size: 1`, a 30s graceful shutdown, a 5m
readiness timeout, a 1m stabilization period, and automatic rollback — an
incremental rollout, one instance at a time. Every one of those timings —
`shutdown.grace-period`, `readiness-timeout`, `stabilization-period` — is a field
you can override; these are only the defaults.

### Capacity and `floor`

Four declarations meet here: the `instances` envelope, the `replace` mode,
volumes, and the cluster's physical capacity. The plan must reconcile them
**before** the first change — any contradiction is a plan error that names the
tension, never a surprise mid-rollout.

`instances.min` describes steady state. A swap rollout *temporarily* lowers
the ready count, and that needs its own explicit consent — `floor`:

```yaml
deploy:
  strategy:
    replace: swap
    floor: 1
```

`floor` is the smallest ready count you accept *during* a rollout. It defaults to
`instances.min` (no consent, no dip below steady state); `floor: 0` accepts full
downtime. Full, all-at-once replacement is not a separate mode — it is one batch
covering everything with `floor: 0`.

**`floor` is a `swap`-only concept.** Under `surge` the ready count never drops —
new instances are added before old ones go — so there is nothing for `floor` to
bound, and it is ignored. It matters only when a `swap` (or a durable
exclusive volume that forces one) takes instances down to replace them; that
is also what `batch` is for, letting you choose how many are briefly unavailable.

```text
swap:    target − batch ≥ floor
surge:   target + batch ≤ instances.max     (floor not involved)
```

The most common corner case — `min: 2`, `preferred: 2`, an exclusive volume —
cannot roll at all without a choice, and the plan says so instead of guessing:

```text
Error: rolling update of application "api" cannot proceed.

  replace:   swap (exclusive volume "data" is durable)
  instances: min 2, target 2, max 4
  floor:     2 (default: instances.min)
  batch:     1

  Taking one instance down leaves 1 ready — below floor 2.

Resolve it explicitly:
  floor: 1                 accept 1 ready instance during rollout
  instances.preferred: 3   add steady-state headroom instead
  floor: 0                 accept downtime

No changes were applied.
```

`surge` needs room for the extra instances, which only the server knows. It is
checked during planning **and** again before each batch. No room is a hard stop
with the shortfall printed — Chmura never waits and hopes space frees up. Surge
is not an exception to `max`: the extra instances count against it like any
other.

### Readiness, stabilization, rollback

`readiness-timeout` is the maximum time to become ready; `stabilization-period` is
how long a batch is watched *after* readiness before moving on. The events that
end that window in failure are a closed list: the runtime exited, the `healthy`
rule killed it, readiness was lost after being gained, or readiness never arrived
in time.

```yaml
deploy:
  strategy:
    rollback: automatic     # or: manual, disabled
    rollback-on: [readiness-failure, runtime-crash, health-failure]
```

- `automatic` — a failed batch reverts every slot changed so far.
- `manual` — the rollout stops and waits for a person.
- `disabled` — the rollout stops and offers no revert.

!!! warning "Rollback restores a revision, never data"
    Rollback restores the previous image and configuration. It does **not**
    restore data. If the new version changed a database schema or an on-disk
    format, reverting the image leaves the old app over new data. Chmura does not
    pretend otherwise — the plan states plainly what a rollback will not cover.
    The mode stays a human choice and is never inferred from the presence of a
    volume.

If the rollback itself fails, the application is left `FAILED`; Chmura attempts no
further reverts and lists exactly which slots stand on which revision. A failed
rollout does not void the revision the deploy created — history should show the
attempt happened.

## Local manifest vs remote state

`chmura.yaml` holds *only* the project's desired state. It never contains runtime
IDs, observed slots, physical bindings, current status, history, allocated
addresses, real resource usage, or overrides.

The server keeps a richer object:

```yaml
metadata:   # stable IDs, revision, timestamps, creator
spec:       # normalized desired state, equivalent to chmura.yaml
overrides:  # explicit imperative choices made outside the manifest
status:     # observed state
```

`chmura project inspect` returns all four; `chmura project export` returns only
`spec` — a file you could save as `chmura.yaml`. Export deliberately omits
overrides, because a manifest by definition does not carry choices made outside
it. Each deploy creates an immutable **revision**; during a rollout, different
slots may run different revisions.

### Imperative commands and the envelope

Every command that changes anything falls into exactly one of three categories,
and every future command must too:

| Category | Examples | Touches desired state? |
| --- | --- | --- |
| operations | `restart`, `drain`, `cancel`, `adopt` | no |
| choice within an envelope | `scale`, `resize` | no — stored as an override |
| deploy | `deploy` | yes — the only thing that changes the envelope |

> The manifest declares the **envelope**. Imperative commands choose a **value
> within it**. Nothing but a deploy changes the envelope.

```bash
chmura app scale api --instances 6     # legal: 2 ≤ 6 ≤ 10
chmura app scale api --instances 12    # error: outside the envelope
```

This is the test for any future command: if it cannot be expressed as a choice
within a declared range, it is not an imperative command — it is a deploy. That
is why there is no `chmura app set-image` or `set-hostname`; image, ports,
mounts, endpoints, and placement have no envelope, so changing them is changing
the envelope.

### Overrides

An imperative choice is recorded explicitly, with who and when:

```text
Application api
  instances
    declared:  min 2, preferred 4, max 10
    override:  6  (daro, 2026-03-14, expires in 1h50m, "traffic spike")
    running:   6
```

An override may carry an **expiry** — a duration or an absolute time — after which
it clears itself and the value returns to its declared envelope. This is exactly
right for a temporary bump like a traffic spike: you set it and forget it, and the
system does not stay scaled up forever because someone forgot to revert:

```bash
chmura app scale api --instances 6 --for 2h
chmura app scale api --instances 6 --until 2026-03-15T08:00Z
```

Without `--for`/`--until` an override is indefinite until cleared. On expiry, the
value simply reverts — the same as a manual `--clear`.

The effective state is `spec` covered by `overrides`. A deploy **does not clear
overrides** — it replaces the envelope and checks the override still fits:

```text
override still within the envelope   → kept, deploy proceeds
override outside the new envelope    → deploy stops
```

Silent reversion is never allowed. That is the one thing this model exists to
prevent: `chmura deploy` quietly undoing a deliberate operational decision.
Clear an override explicitly with `chmura app scale api --clear` or
`chmura project overrides clear`.

An override creates no revision — a revision tracks the immutable manifest, and an
override does not change the manifest. So `chmura app scale` on production does
**not** make a colleague's deploy fail on a revision conflict; a conflict signals
a real manifest change by someone else, not every operation along the way.

Rollback is the exception on the other side: it changes the active revision, so
it is an envelope change — to an earlier, existing one. Afterward the manifest in
your repository still describes the version that failed, so an identical
`chmura deploy` would ship it again and asks for confirmation first.
