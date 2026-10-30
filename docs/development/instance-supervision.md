# Instance supervision

`internal/engine/k3s.Supervisor.Monitor` joins runtime observation and the
readiness/healthy monitors for one explicitly selected running container
generation. The owner supplies its portable plan and retained `EverReady` fact.
The resolver and network runner use explicit clients/options; no local
kubeconfig or implicit instance selection is involved.

Checks start only after the runtime snapshot is joined to a watch. Readiness
uses the actual process start time, the full consecutive thresholds and any
known prior first readiness. An independent healthy check starts after first
readiness, or immediately when that fact is already known for this generation.
Losing readiness does not pause or reset healthy monitoring. An omitted healthy
rule or process-only healthy rule schedules no extra probe. Readiness failures
alone do not stop a previously ready process.

The first terminal worker cause cancels the observation and both monitors. The
method waits for their cleanup, including active network dial callbacks, before
returning. Parent cancellation/deadline takes precedence on return. Causes retain
their meaning: `ErrStartupTimeout`, `ErrUnhealthy`, runtime unavailability or
replacement, and `ErrObservationLost` are distinct. In particular, losing a watch
is not evidence of an application crash or a failed health check.

Readiness callbacks run serially, publish the initial state and changes, and
must return promptly without outbound I/O. Any current readiness is revoked
locally when monitoring ends, while its `EverReady` fact is retained. This
invalidation is not an assertion that a probe failed: the caller must also
inspect the returned cause. No callbacks run after the method returns.

The initial loop supports explicit HTTP/TCP readiness and optional independent
HTTP/TCP or process-only healthy monitoring. Unsupported checks fail before
observation. It does not restart, reconnect, adopt another generation, discover
a container ID, or publish status. The caller must retain generation history and
reconcile a gap explicitly; native Kubernetes readiness cannot supply that
history. A generation already declared failed must not be resumed.

The next layer is a serialized reporting/reconciliation worker around the
[readiness writer](probe-execution.md#readiness-publication). It must decouple
probe callbacks from outbound I/O and reconcile current state after conflicts or
failures. Durable history, distributed supervisor ownership, restart execution,
agent wiring and API/CLI deployment remain separate increments. Tests join real
typed clients to local HTTP API/watch and application servers; a live k3s
deployment is not yet verified.
