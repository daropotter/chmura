# `chmura.yaml` reference

`chmura.yaml` is the portable definition of a project: its applications, volumes,
and endpoints. It holds **desired state only** — never a space, remote, tokens,
cluster, runtime IDs, or status. It belongs in Git. Where it deploys comes from a
[remote and space](../cli.md#addresses), not from this file.

!!! note "Draft"
    The model is settled; this reference is being filled in. Two field-level
    questions are still open and marked **Under review** below.

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
- **Scalar-or-range.** A sizing field takes a `min`/`preferred`/`max` object, and
  a bare scalar is shorthand for a fixed range (`memory: 512Mi` ⇒
  `min = preferred = max = 512Mi`).

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

One `check` mechanism, two rules over its results. Full semantics in
[Deployment](../deployment.md#health-checks).

```yaml
health:
  check:
    http: { path: /healthz, port: http }   # or tcp:/exec:
    interval: 5s
    timeout: 2s
  ready:   { after-successes: 1, lost-after-failures: 3 }
  restart: { after-failures: 6 }
  startup-timeout: 2m
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `check` | object | — | Probe mechanism: one of `http` · `tcp` · `exec`. Omitting `health` entirely means `process-only` readiness. |
| `check.interval` | duration | — | Time between probes. |
| `check.timeout` | duration | — | When a single probe counts as failed. |
| `ready.after-successes` | integer | `1` | Consecutive successes that grant readiness. |
| `ready.lost-after-failures` | integer | `3` | Consecutive failures that revoke it. |
| `restart` | object | *(absent)* | Opt-in. Absent means the check never kills. May carry its own `check`. |
| `restart.after-failures` | integer | — | Consecutive failures that kill the instance. Must exceed `ready.lost-after-failures` (validation error otherwise). |
| `startup-timeout` | duration | — | Max time to first readiness before the instance is failed. |

Check forms:

```yaml
http: { path: /healthz, port: http, expect-status: [200] }
tcp:  { port: signaling }
exec: { command: ["/bin/check-queue"] }    # exit 0 = healthy
```

A check names a **port by name**, never a number.

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
    rollback:
      mode: automatic       # or: manual, disabled
      on: [readiness-failure, runtime-crash, restart-triggered]
```

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `replace` | enum | `surge` | `surge` (new ready before old stops) · `swap` (old stops first). An `attachment: exclusive` volume forces `swap`. |
| `batch.size` | integer | `1` | Fixed instances per batch. |
| `batch.percentage` | integer | — | Percent per batch, rounded up, min 1. |
| `batch.partitions` | integer | — | Split instances into N roughly equal batches. |
| `floor` | integer | `instances.min` | Smallest ready count tolerated *during* rollout. `0` accepts downtime. |
| `shutdown.grace-period` | duration | `30s` | Graceful shutdown window. |
| `readiness-timeout` | duration | `5m` | Max time to become ready. |
| `stabilization-period` | duration | `1m` | Observation after readiness before the next batch. |
| `rollback.mode` | enum | `automatic` | `automatic` · `manual` · `disabled`. |
| `rollback.on` | list | — | Triggers: `readiness-failure`, `runtime-crash`, `restart-triggered`. |

`batch.size`, `batch.percentage`, and `batch.partitions` are mutually exclusive.

!!! note "Under review — `strategy.mode`"
    Prose elsewhere mentions `mode: rolling | all-at-once`, but the relationship
    to `batch` and `floor` is unresolved: `all-at-once` may just be
    `batch` covering everything plus `floor: 0`, in which case `mode` is
    redundant. Pending a decision, only `replace`/`batch`/`floor` are documented
    as the rollout controls. See the findings note in the changelog.

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
| `allocation` | enum | — | `shared` (one logical volume for all slots) · `per-instance` (one per slot). |
| `attachment` | enum | — | `exclusive` (one runtime at a time) · `concurrent` (many; needs a `multi-attach` pool). |
| `size` | range | *(elastic)* | See below. |
| `storage` | object | — | Policy and tag requirements. |
| `lifecycle` | object | — | Detach retention and protection. |
| `reset` | enum | `never` | `never` (persists) · `on-deploy` (fresh volume each deploy). |

### `size`

Optional. Omitting it means an **elastic** volume — starts at the pool's smallest
unit and grows on demand. Providing it means a bounded envelope. For
`per-instance`, the range is **per instance**, never a total.

```yaml
size: { min: 70Gi, preferred: 100Gi, max: 200Gi }
```

!!! note "Under review — scalar `size` shorthand"
    Resources allow `memory: 512Mi` as scalar shorthand for a fixed range. Whether
    `size: 50Gi` is allowed as the same shorthand for volumes is not yet decided.
    Only the object form and omission are documented for now.

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
      ready:   { lost-after-failures: 3 }
      restart: { after-failures: 6 }
      startup-timeout: 2m
    mounts:
      uploads: { volume: uploads, path: /var/lib/uploads }

volumes:
  uploads:
    allocation: shared
    attachment: concurrent
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
