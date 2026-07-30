# Observability

[`status`](deployment.md) answers "how are things right now?" Observability adds
the dimension of time: what happened, how values changed, what an application
emitted.

None of this reinvents state. The same observation `status` shows as a snapshot
becomes a series over time; the same transitions it shows as current become
events over time.

Signals split by **who produces them** — exactly as readiness splits by who knows
the state (see [Deployment](deployment.md)):

| Producer | Signals | Captured |
| --- | --- | --- |
| Chmura, always | events, platform metrics, rollout and probe state | automatic |
| the app, captured automatically | logs (stdout/stderr) | automatic |
| the app, only if declared | application metrics, traces | opt-in |

## The four signals

| Signal | What it is | Source |
| --- | --- | --- |
| logs | the app's stdout/stderr stream | captured automatically |
| events | the domain timeline | produced by Chmura |
| metrics | numeric series | platform (auto) + application (opt-in) |
| traces | request paths between applications | application (opt-in) |

## Events

This is the most valuable and most native signal, because Chmura produces it
either way. Operations, revisions, rollout state, degradations, autoscaling,
certificate renewals, node events — all of it already happens inside. `chmura
events` exposes that stream addressably.

```text
deploy started              revision 15 created
rollout advanced            batch 2/4 ready
instance restarted          api/1, reason: unhealthy
volume degraded             uploads allocated 72Gi of 100Gi preferred
override set                api instances 6 (daro)
certificate renewed         shop.example.com
node drained                worker-2
```

```bash
chmura events company:production/shop --since 1h
chmura events company:production/shop/api --type restart
chmura events company:                     # installation events: nodes, pools
```

Events are structured, and their scope follows the address:

| Scope | Examples |
| --- | --- |
| application | restart, lost readiness, instance degradation |
| project | deploy, rollout, rollback, override change |
| space | secret rotation, migration |
| installation | node join and drain, pool pressure, certificate renewal |

Events are also the natural hook for auditing: the "who and when" of every state
change is recorded in them (`created-by`).

## Metrics

**Platform metrics** are known to Chmura because it runs the workload. They are
always available, with no declaration:

```text
CPU and memory use against min / preferred / max
ready instances against desired
restarts, readiness transitions
volume usage against preferred
rollout progress and state
```

**Application metrics** the app must expose itself. Chmura does not know them
until they are declared — and then it scrapes them from a
[named port](networking.md):

```yaml
applications:
  api:
    observability:
      metrics:
        port: metrics          # a named port, never a number
        path: /metrics
        format: prometheus
```

No declaration means no application metrics — and `status` and `doctor` say so
plainly, exactly as with `readiness: process-only`:

```text
Application api
  metrics: platform-only (no application metrics declared)
```

We do not forbid it, but we do not let thin visibility look complete — the same
principle as a pool's `capacity` and as degradation.

```bash
chmura metrics company:production/shop/api
chmura metrics company:production/shop/api --output json
```

## Logs

Logs (stdout/stderr) are captured **automatically**, with no declaration — the
one application signal that needs no opt-in, because it asks nothing of the app
beyond writing to standard output.

`chmura logs` is a project operation. Flags narrow the scope:

```bash
chmura logs company:production/shop --follow
chmura logs company:production/shop/api --since 15m --tail 200
```

| Flag | Meaning |
| --- | --- |
| `--follow`, `-f` | live stream (server-sent events) |
| `--since` | from a given time or duration |
| `--tail` | the last N lines |
| `--output json` | one JSON object per entry (ndjson), for further processing |

Chmura forwards logs **verbatim** and does not scrub secrets from them. It cannot
be done reliably, and pretending logs are safe would be worse than the plain
statement: not logging secrets is the application's responsibility.

## Traces

Traces are entirely an application signal — they require instrumentation in the
app. Chmura's role is narrow: it propagates trace context at the
[edge proxy](networking.md) and forwards spans to the sink.

```yaml
applications:
  api:
    observability:
      traces:
        protocol: otlp
```

Because the weight is on the application and the OTLP standard, the details are
deferred. The manifest hook exists from the start so that turning traces on is
never a breaking change.

## Storage: our interface, a pluggable backend

Chmura **does not build its own observability stack**. The same principle holds
as for secrets and the execution engine:

!!! quote
    The user-facing interface is ours and stable. The backend that stores and
    analyzes signals is replaceable.

The built-in store is **bounded by a retention window** — enough for a
single-node installation to work: `chmura logs`, `events`, `metrics`, and
`doctor` over a short horizon. Durable retention, dashboards, and alerting are an
external sink, wired in by an administrator:

```yaml
# installation.yaml
observability:
  retention:                 # built-in, bounded
    events: 30d
    logs: 3d
    metrics: 24h

  forward:                   # optional external sinks
    metrics: { otlp: https://collector.example/v1/metrics }
    logs:    { otlp: https://collector.example/v1/logs }
    traces:  { otlp: https://collector.example/v1/traces }
```

With no `forward` configured, only the built-in window exists. What falls out of
it is gone — and that is stated plainly, not hidden behind the pretense of
unlimited history.

The CLI reads observability **only through the API**, never directly from the
sink. The sink is an installation detail, like the engine or the secret driver —
see [Architecture](architecture.md).

## Alerting

`chmura doctor` answers, synchronously, "is anything wrong right now?" Events are
the asynchronous record of what happened.

Alerting — notifying when a condition is met — is not built into Chmura. It is a
natural function of the external sink, driven by the forwarded stream of events
and metrics, and remains deferred.
