# `chmura.yaml` reference

`chmura.yaml` is the portable definition of a project: its applications, volumes,
and endpoints. It holds **desired state only** — never a space, remote, tokens,
cluster, runtime IDs, or status. It belongs in Git. Where it deploys comes from a
[remote and space](../cli.md#addresses), not from this file.

!!! note "Draft"
    The model is settled; this reference is being filled in as the other config
    files are documented.

## Naming conventions

Every Chmura-defined key and enum value — here and in the other config files
(`chmura.dev.yaml`, `cluster.yaml`, `installation.yaml`) — is **kebab-case**:
`on-deploy`, `min-age`, `startup-timeout`, `per-instance`, `multi-attach`. This
matches the CLI, whose flags are kebab-case too (`--fail-on-degraded`), so the
same concept is spelled the same in a flag and in a manifest.

Three things follow their own conventions:

- **Environment variable names** are `UPPER_SNAKE_CASE` — the operating system's
  convention, not Chmura's (`DB_PASSWORD`).
- **Names you choose** — applications, ports, volumes, endpoints, values — are
  yours; examples use lowercase.
- **External identifiers** — hostnames, image references, file paths — follow
  their own domains.

## Value forms

Several fields accept more than one shape. Two patterns recur, so they are named
once here:

- **Scalar-or-reference.** A value field takes a literal, or an object
  referencing a stored value: `{ var: <name> }` or `{ secret: <name> }`.
  References resolve project-then-space at deploy time; see
  [values](../getting-started/first-steps.md).
- **Scalar-or-range.** A sizing field takes a `min`/`preferred`/`max` object, or a
  bare scalar for a fixed value (`memory: 512Mi` ⇒ `min = preferred = max`). The
  same rule covers `resources`, `instances`, and volume `size` — there is no
  field-specific sizing syntax. See [Ranges](#ranges).

## Ranges

A range object may omit keys; the rest are filled by a fixed set of rules, so you
rarely write all three:

| Given | Filled to |
| --- | --- |
| `max` only | `min = preferred = max` (fixed at max) |
| `preferred` only | `min = preferred`, `max = preferred` (fixed at preferred) |
| `min` only | `preferred = min`, `max = min` (fixed at min) |
| `min` + `max` | `preferred = min` (aim low, scale toward max) |
| `preferred` + `max` | `min = preferred` |
| `min` + `preferred` | `max = preferred` (no scaling above preferred) |

Two rules make this safe:

- **`max` is never unbounded.** Omitting it defaults `max` to `preferred`.
  Unbounded scaling would be uncontrolled, non-deterministic behavior, so it is
  not allowed.
- `min ≤ preferred ≤ max` must hold; a contradictory range is a validation error.

Omitting a range object entirely is allowed only where a default exists
(`instances` defaults to `1`) or where the target permits it — see volume
[`size`](#size), which may be elastic.

## Top level

| Key | Type | Required | Purpose |
| --- | --- | --- | --- |
| `version` | integer | yes | Manifest schema version. Currently `1`. |
| `name` | string | yes | Project name — the recognition key within a space (with the space itself). Written explicitly by `chmura init`; never recomputed at deploy. Renaming is `chmura project rename`, not an edit here. |
| `applications` | map | yes | One or more applications, keyed by name. |
| `volumes` | map | no | Logical volumes, keyed by name. |
| `endpoints` | map | no | Public endpoints, keyed by name. |

## `applications.<name>`

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `source` | object | — | Where the image comes from. See below. |
| `instances` | range | `1` | Instance count envelope. Scalar-or-range. |
| `resources` | object | — | CPU and memory envelopes. |
| `ports` | map | — | Named ports. |
| `env` | map | — | Environment variables. Application-level only; no inheritance or merging. |
| `files` | map | — | Files materialized into the container. |
| `health` | object | — | Health check and its rules. |
| `mounts` | map | — | Volume mounts. |
| `placement` | object | — | High-availability spread rules. |
| `network` | object | egress allowed | Egress policy. |
| `observability` | object | platform-only | Application metrics and traces. |
| `deploy` | object | see [defaults](#deploystrategy) | Rollout strategy. |
| `slots` | object | `numbering: reuse` | Slot numbering. |

### `source`

Exactly one of two forms:

```yaml
source:
  image: registry.example.com/api@sha256:...   # a prebuilt image
```

```yaml
source:
  context: .            # build locally from a Dockerfile
  dockerfile: Dockerfile
```

| Key | Type | Purpose |
| --- | --- | --- |
| `image` | string | Immutable image reference (digest preferred). |
| `context` | path | Build context directory. |
| `dockerfile` | path | Dockerfile within the context. |

`image` and `context` are mutually exclusive.

### `instances`

Scalar-or-range. `min` — below it the project is unsatisfied; `preferred` — the
target Chmura aims for; `max` — the ceiling for scaling and autoscaling.

```yaml
instances: { min: 2, preferred: 4, max: 10 }
```

### `resources`

```yaml
resources:
  cpu:    { min: 100m, preferred: 500m, max: 2000m }
  memory: 512Mi        # scalar shorthand for a fixed range
```

`cpu` and `memory` are each scalar-or-range. An instance given at least `min` but
below `preferred` runs **degraded** (shown in status), not failed.

### `ports.<name>`

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `number` | integer | — | The port the app listens on. |
| `protocol` | enum | — | `http` · `https` · `tcp` · `udp`. `http`/`https` are L7 (multiplexable by hostname); `tcp`/`udp` are L4 (exclusive). |
| `visibility` | enum | `project` | `application` · `project` · `space` · `public`. |

See [Networking](../networking.md) for what each protocol and visibility means.

### `env.<VAR>`

Scalar-or-reference. On a `secret`/`var` reference, `on-change` controls what
happens when the stored value changes:

```yaml
env:
  LOG_LEVEL: info                          # literal
  API_URL:      { var: api-url }           # var reference
  DB_PASSWORD:  { secret: db-password, on-change: restart }
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `on-change` | enum | `ignore` | `ignore` (only flag drift) · `restart` (roll the app per its strategy). |

### `files.<path>`

A stored value materialized as a file before the process starts. Not a volume —
recreated each start, never persisted.

```yaml
files:
  /etc/api/tls.pem:    { secret: api-tls-cert, mode: "0400" }
  /etc/api/client.json: { var: client-config }
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `secret` / `var` | string | — | The stored value to materialize (exactly one). |
| `mode` | string | `0400` secret, `0444` var | File permission bits. |

### `health`

Readiness and health are two different questions with two rules;
[Deployment](../deployment.md#health-checks) has the full semantics and the
reasoning. `health.check` is the **readiness** probe (may reach a dependency);
`healthy` is the **health** rule (the process only, unless given its own check).

```yaml
health:
  check:                      # the readiness probe
    http: { path: /ready, port: http }   # or tcp:/exec:
    interval: 5s
    timeout: 2s
  ready:   { unready-failures: 3 }
  healthy: { unhealthy-failures: 6 }     # opt-in; process-only by default
  startup-timeout: 2m
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `check` | object | — | The readiness probe: one of `http` · `tcp` · `exec`. Omitting `health` entirely means `process-only` readiness. |
| `check.interval` | duration | — | Time between probes. |
| `check.timeout` | duration | — | When a single probe counts as failed. |
| `ready.successes` | integer | `1` | Consecutive successes that grant readiness. |
| `ready.unready-failures` | integer | `3` | Consecutive failures that revoke it (out of traffic). |
| `healthy` | object | *(absent)* | Opt-in health rule. Absent means only a process crash restarts. Watches the process only unless given its own `check`; **never** the readiness check. |
| `healthy.unhealthy-failures` | integer | — | Consecutive failures that kill and restart the instance. |
| `healthy.check` | object | *(process)* | Optional own probe — keep it local to the process, never a downstream. |
| `startup-timeout` | duration | — | Max time to first readiness before the instance is failed. |

Check forms:

```yaml
http:    { path: /healthz, port: http, expect-status: [200] }
tcp:     { port: signaling }
process: { name: myapp }                   # a named process/executable is running
exec:    { command: ["/bin/check-queue"] } # exit 0 = healthy
```

An `http`/`tcp` check names a **port by name**, never a number. A container crash
is always restarted with no check at all; the `healthy` rule and the `process`
check catch a process that runs but is wedged.

### `mounts.<name>`

```yaml
mounts:
  uploads: { volume: uploads, path: /var/lib/uploads }
```

| Key | Type | Purpose |
| --- | --- | --- |
| `volume` | string | Name of a volume declared in `volumes`. |
| `path` | path | Mount path inside the container. |

A volume is not visible to an application until it is mounted.

### `placement`

```yaml
placement:
  spread:
    across: locations
    minimum-locations: 2
    minimum-per-location: 1
```

| Key | Type | Purpose |
| --- | --- | --- |
| `spread.across` | enum | `locations` (the only value in v1). |
| `spread.minimum-locations` | integer | Distinct locations the app must run in. |
| `spread.minimum-per-location` | integer | Minimum instances per used location. |

### `network`

```yaml
network:
  egress:
    internet: true      # default; set false to deny
```

### `observability`

```yaml
observability:
  metrics: { port: metrics, path: /metrics, format: prometheus }
  traces:  { protocol: otlp }
```

| Key | Type | Purpose |
| --- | --- | --- |
| `metrics.port` | string | Named port to scrape. Absence means platform-only metrics. |
| `metrics.path` | path | Scrape path. |
| `metrics.format` | enum | `prometheus`. |
| `traces.protocol` | enum | `otlp`. Details deferred; see [Observability](../observability.md). |

### `deploy.strategy`

```yaml
deploy:
  strategy:
    replace: surge          # or: swap
    batch: { size: 1 }      # size xor percentage xor partitions
    floor: 1                # default: instances.min
    shutdown: { grace-period: 30s }
    readiness-timeout: 5m
    stabilization-period: 1m
    rollback: automatic     # or: manual, disabled
    rollback-on: [readiness-failure, runtime-crash, health-failure]
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `replace` | enum | `surge` | `surge` (new ready before old stops) · `swap` (old stops first). A durable `per-instance` volume forces `swap` (see [Deployment](../deployment.md)). |
| `batch.size` | integer | `1` | Fixed instances per batch. |
| `batch.percentage` | integer | — | Percent per batch, rounded up, min 1. |
| `batch.partitions` | integer | — | Split instances into N roughly equal batches. |
| `floor` | integer | `instances.min` | Smallest ready count tolerated *during* rollout. `0` accepts downtime; it replaces the old "all-at-once" mode — one batch covering everything with `floor: 0`. |
| `shutdown.grace-period` | duration | `30s` | Graceful shutdown window. |
| `readiness-timeout` | duration | `5m` | Max time to become ready. |
| `stabilization-period` | duration | `1m` | Observation after readiness before the next batch. |
| `rollback` | enum | `automatic` | `automatic` · `manual` · `disabled`. |
| `rollback-on` | list | *(defaults)* | Triggers: `readiness-failure`, `runtime-crash`, `health-failure`. |

`batch.size`, `batch.percentage`, and `batch.partitions` are mutually exclusive.
There is no `mode` key on the strategy: an incremental rollout is `batch` +
`floor`, and full replacement is one batch with `floor: 0`.

### `slots`

```yaml
slots:
  numbering: serial    # default: reuse
```

`reuse` (compact numbering, lowest free slot reclaimed) or `serial` (numbers only
increase, never reused). See [Storage](../storage.md).

## `volumes.<name>`

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `allocation` | enum | — | `shared` (one volume, all instances, concurrent; needs a `multi-attach` pool) · `per-instance` (one volume per slot, exclusive). There is no separate `attachment` — a single-writer volume is `per-instance` with `instances: 1`. |
| `size` | range | *(elastic)* | See below. |
| `storage` | object | — | Policy and tag requirements. |
| `lifecycle` | object | — | Detach retention and protection. |
| `reset` | enum | `never` | `never` (persists) · `on-deploy` (fresh volume each deploy). |

### `size`

A [range](#ranges) like every other sizing field — scalar for a fixed size,
object for a range. For `per-instance`, the range is **per instance**, never a
total.

```yaml
size: { min: 70Gi, preferred: 100Gi, max: 200Gi }
size: 50Gi                    # fixed
```

Omitting `size` entirely requests an **elastic** volume — it starts at the pool's
smallest unit and grows on demand. This requires a pool that can grow (a
directory/path pool, or one with the resize capability). If the target pool
cannot grow, an omitted size is an error asking for an explicit one — Chmura does
not silently pick a fixed size. See [Storage](../storage.md).

### `storage`

```yaml
storage:
  policy: fast
  tags:
    required:  [ha, backup]
    preferred: [nvme]
    forbidden: [experimental]
  fallback:
    policies: [balanced, cheap]
```

| Key | Type | Purpose |
| --- | --- | --- |
| `policy` | string | Name of an installation storage policy. |
| `tags.required` | list | All must be present, or it is an error. Summed with the policy's. |
| `tags.preferred` | list | Absence allows degradation, not failure. |
| `tags.forbidden` | list | Presence rejects a pool. |
| `fallback.policies` | list | Ordered fallbacks; may not break `required`. |

A project may only *tighten* a policy's requirements, never relax them.

### `lifecycle`

```yaml
lifecycle:
  protect: true
  detached:
    policy: pressure      # retain · expire · pressure
    min-age: 30d
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `protect` | bool | `false` | Excludes the volume from automatic cleanup. |
| `detached.policy` | enum | — | `retain` (never auto-deleted) · `expire` (deletable after `min-age`) · `pressure` (reclaimable after `min-age`, deleted only under pressure, FIFO). |
| `detached.min-age` | duration | — | Minimum detached age before `expire`/`pressure` applies. |

## `endpoints.<name>`

```yaml
endpoints:
  website:
    ingress: default            # optional; default "default"
    target:   { application: api, port: http }
    listen:   { protocol: https, port: 443, hostname: api.example.com }
    tls:      { mode: terminate, certificate: automatic }
    affinity: client-ip         # optional
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `ingress` | string | `default` | Named cluster ingress to attach to. |
| `target.application` | string | — | Application to route to. |
| `target.port` | string | — | Named port on that application. |
| `listen.protocol` | enum | — | `http` · `https` · `tcp` · `udp`. |
| `listen.port` | integer | — | External port. |
| `listen.hostname` | string or `{ var }` | — | Hostname to match. Scalar-or-reference (var only, never secret). Required for `http`/`https`; absent for `tcp`/`udp`. |
| `tls` | object | — | Present only for `https`. See below. |
| `affinity` | enum | none | `client-ip` · `cookie`. Session stickiness. |

### `tls`

Present **only** for `listen.protocol: https`.

| Key | Type | Purpose |
| --- | --- | --- |
| `mode` | enum | `terminate` (Chmura decrypts, holds the cert) · `passthrough` (app holds it). |
| `certificate` | scalar or object | `automatic`, or `{ secret: <name> }`. Required for `terminate`. |

The conflict key that keeps endpoints unique is `(ingress, port, hostname)` for
`http`/`https` and `(ingress, port)` for `tcp`/`udp` — see
[Networking](../networking.md#uniqueness-and-conflicts).

## Complete example

```yaml
version: 1
name: shop

applications:
  api:
    source:
      image: registry.example.com/shop-api@sha256:...
    instances: { min: 2, preferred: 4, max: 10 }
    ports:
      http: { number: 8080, protocol: http, visibility: project }
    env:
      LOG_LEVEL: info
      DB_PASSWORD: { secret: db-password, on-change: restart }
    health:
      check:   { http: { path: /healthz, port: http } }
      ready:   { unready-failures: 3 }
      healthy: { unhealthy-failures: 6 }
      startup-timeout: 2m
    mounts:
      uploads: { volume: uploads, path: /var/lib/uploads }

volumes:
  uploads:
    allocation: shared
    size: { min: 70Gi, preferred: 100Gi, max: 200Gi }
    storage:
      policy: balanced
      tags: { required: [ha, backup], preferred: [ssd] }

endpoints:
  api:
    target: { application: api, port: http }
    listen: { protocol: https, port: 443, hostname: api.shop.example.com }
    tls:    { mode: terminate, certificate: automatic }
```
