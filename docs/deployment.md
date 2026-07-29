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

A new instance with a `per-instance` volume needs room for its own volume, so
scaling up follows the same rule as creating one — and never forces it:

```text
fits preferred          instance starts, volume at preferred
fits at least min       instance starts, volume DEGRADED, warning
does not fit min         scaling that instance fails
```

A scale-up that cannot place a volume at `min` stops at that instance rather than
squeezing it in somewhere. See [Storage](storage.md) for volume placement.

## Health checks

Three questions with different consequences:

| Question | Answered by | |
| --- | --- | --- |
| How, and how often, do we ask the app? | `check` | the mechanism |
| May this instance take traffic? | `ready` | a rule over results |
| Must this instance be killed? | `restart` | a rule over results |

Other orchestrators define a separate probe for each and make you repeat the
mechanism. In practice the mechanism is one; only the thresholds and consequences
differ. Chmura separates the layers: a `check` **produces a stream of results**
and means nothing on its own — the rules give it meaning.

```yaml
applications:
  api:
    health:
      check:
        http:
          path: /healthz
          port: http
        interval: 5s
        timeout: 2s
      ready:
        afterSuccesses: 1
        lostAfterFailures: 3
      restart:
        afterFailures: 6
      startupTimeout: 2m
```

This structure lets Chmura enforce a dependency other systems leave to chance:

!!! warning "Killing is never faster than removing from traffic"
    With a shared check, `restart.afterFailures` must be greater than
    `ready.lostAfterFailures`. The reverse — an instance killed before it stops
    receiving traffic — is a validation error, not something you can misconfigure.

### Check types

```yaml
http:  { path: /healthz, port: http, expectStatus: [200] }
tcp:   { port: signaling }
exec:  { command: ["/bin/check-queue"] }
```

`http` for HTTP services, `tcp` for services that do not speak HTTP (databases,
TURN, binary protocols), `exec` for everything else including port-less workers
(exit `0` means healthy). A check names a **port by name**, never a number.
`exec` is also the extension point: whatever an app considers "healthy" is a
script, not a new manifest field.

### `ready`

```text
afterSuccesses      consecutive successes that grant readiness   default 1
lostAfterFailures   consecutive failures that revoke it          default 3
```

Readiness drives both traffic and rollout progress. The defaults suit a typical
HTTP service, so a declaration is often just `path` and `port`.

### `restart` is a deliberate choice

Omitting `restart` means **the check never kills an instance**. This is not a
gap: process exit is observed always, with no check at all — a crash is restarted
in the same slot. The `restart` rule exists only to catch *hangs* — a process
that is alive but no longer responding. Most apps never need it, and misused it
turns a blip into a restart storm, so it is opt-in.

It does not apply until `ready` has succeeded once; if readiness never arrives
within `startupTimeout`, the instance is considered failed. That removes the need
for a separate startup probe.

!!! warning "Restart depends only on the process"
    The classic mistake: a readiness check probes a dependency (a database), and
    the same check drives killing. The database blips and *every* instance is
    killed at once — a restart storm exactly when the system is already
    struggling. So: readiness may depend on dependencies; restart must depend
    only on the process itself. If the shared check reaches a dependency, give
    `restart` its own local, cheap check:

    ```yaml
    restart:
      afterFailures: 6
      check:
        exec:
          command: ["/bin/self-check"]
    ```

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
surge         a new instance starts and becomes ready before the old one stops;
              the ready count never drops
stop-first    the old instance stops before the new one starts;
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
Where it is infeasible, the plan says so and requires an explicit `stop-first`:
lowering the ready count during a rollout should be visible in the manifest and
in review, not inferred silently.

### Volume handover forces stop-first

A volume with `attachment: exclusive` can be attached to one runtime at a time,
so the old runtime must release it before the new one starts. An app with such a
volume can only replace `stop-first`. Declaring `replace: surge` with an
exclusive volume is a validation error that tells you exactly how to resolve it.
There is no separate `handover` field — the old `handover: exclusive` is just
`replace: stop-first`. See [Storage](storage.md).

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
strategy is rolling, `replace: surge`, `batch.size: 1`, a 30s graceful shutdown,
a 5m readiness timeout, a 1m stabilization period, and automatic rollback.

### Capacity and `floor`

Four declarations meet here: the `instances` envelope, the `replace` mode,
volumes, and the cluster's physical capacity. The plan must reconcile them
**before** the first change — any contradiction is a plan error that names the
tension, never a surprise mid-rollout.

`instances.min` describes steady state. A stop-first rollout *temporarily* lowers
the ready count, and that needs its own explicit consent — `floor`:

```yaml
deploy:
  strategy:
    replace: stop-first
    floor: 1
```

`floor` is the smallest ready count you accept *during* a rollout. It defaults to
`instances.min` (no consent, no dip below steady state); `floor: 0` accepts full
downtime and is required by `mode: all-at-once`.

```text
stop-first:   target − batch ≥ floor
surge:        target + batch ≤ instances.max
```

The most common corner case — `min: 2`, `preferred: 2`, an exclusive volume —
cannot roll at all without a choice, and the plan says so instead of guessing:

```text
Error: rolling update of application "api" cannot proceed.

  replace:   stop-first (volume "data" is attachment: exclusive)
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

`readinessTimeout` is the maximum time to become ready; `stabilizationPeriod` is
how long a batch is watched *after* readiness before moving on. The events that
end that window in failure are a closed list: the runtime exited, the `restart`
rule killed it, readiness was lost after being gained, or readiness never arrived
in time.

```yaml
rollback:
  mode: automatic     # or: manual, disabled
  on: [readiness-failure, runtime-crash, restart-triggered]
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
    override:  6  (daro, 2026-03-14, "traffic spike")
    running:   6
```

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
