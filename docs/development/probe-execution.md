# Probe execution

`internal/probe.Network.Run` performs one HTTP or TCP attempt against a runtime
instance. A runtime adapter supplies the instance host and the application's
declared named ports. The runner does not select an instance, expose an endpoint,
or contact the control plane.

Each invocation uses `manifest.Health.Effective` to resolve omitted timeout and
HTTP success-status settings. HTTP sends a GET to the declared path on the
selected application port, including its query string. Redirects are never
followed, including when a redirect status is explicitly accepted. Success is
determined by status; the response body is closed without reading an unbounded
application response. TCP success means a connection was established; the
connection is then closed.

Requests connect directly rather than using environment proxy settings. HTTPS
uses normal certificate and hostname verification. A runtime adapter may supply
explicit CA roots and an expected TLS server name. There is no certificate
verification bypass. Dialing can be supplied by the adapter for its network
context, and the dialer must honor cancellation.

The attempt deadline covers dialing, TLS negotiation and HTTP response headers.
Parent cancellation and an earlier parent deadline take precedence. Timeout or
cancellation closes outstanding network work; errors retain their context cause
for `errors.Is`. An unexpected HTTP response returns `StatusError` with its code.

`probe.Runtime.Run` performs a process or exec attempt through callbacks bound
to one runtime instance. The adapter supplies `ProcessRunning` for a named
process/executable and `Exec` for a literal executable/argument vector. It must
execute that vector directly without adding a shell, honor cancellation of the
entire operation, and finish runtime I/O and process cleanup before returning.
Only the callback for the selected probe type is required; a missing adapter is
an error. The runner has no default host implementation.

A running named process succeeds; an absent one returns `ErrProcessNotRunning`.
An exec exit status of zero succeeds, while any nonzero status returns
`ExitError` with its code. Adapter execution errors retain their cause for
`errors.Is`. The runner copies the declared arguments and resolves the shared
attempt timeout, including the `2s` default. Parent stop or an earlier deadline
takes precedence over the returned outcome, even if an adapter returns success
at its deadline. The call waits for adapter cleanup before returning.

`internal/engine/k3s.Resolver.Resolve` obtains the network target and process
start time for an explicitly selected container generation. An engine reference
contains namespace, pod name/UID, container name and container ID; these remain
private adapter facts. The resolver uses an explicitly configured `client-go`
client and does not load local kubeconfig or create cluster resources.

Resolution requires a running, non-terminating pod and the selected running
application container. Pod or container identity changes return
`ErrStaleInstance`; missing runtime facts return `ErrInstanceUnavailable`.
Kubernetes readiness flags are deliberately ignored so Chmura can evaluate its
own readiness rules. The result uses the primary pod IP, an independent copy of
the application's declared ports, and the container's actual start time rather
than pod creation/image-pull time. API errors retain their cause, and the caller
owns the lookup deadline.

This is a point-in-time observation. Resolution does not pin an IP to a process
generation; lifecycle supervision must cancel checks when that generation exits
or is replaced. Concrete process/exec operations and agent wiring follow later.

`Resolver.Observe` joins that snapshot to a name-filtered watch starting from
its resource version. Once the watch opens, it calls `onStart` once with the
snapshot target; the callback must return promptly. The owner can start checks
there, then cancel them and wait for cleanup when observation returns. A
deleted/non-running instance returns `ErrInstanceUnavailable`, while a changed
pod UID or container ID returns `ErrStaleInstance`. Readiness changes, bookmarks
and repeated valid events do not restart the observer or the checks.

Watch closure, API errors (including an expired resource version), invalid events
or changed target address/start time return `ErrObservationLost` and preserve
any API error cause. This means observation continuity is unknown, rather than
that the application failed a health check. The observer closes its stream,
does not reconnect or adopt a new generation, and leaves explicit reconciliation
to the owner. Parent stop retains its context cause. Neither snapshot nor watch
pins an IP to a generation or makes runtime event delivery instantaneous.

`probe.Schedule` serially invokes an attempt function for one check. It starts
immediately and waits at least one interval from each completion before the next
attempt. Results retain their completion time even if delivery is delayed. The
caller supplies a result channel, which is never closed by the scheduler; a slow
consumer applies backpressure without an internal queue or catch-up bursts.

Attempt errors are published as outcomes, and monitoring continues. Parent
cancellation stops publication and further attempts, signals the active attempt
and waits for its cleanup. An attempt interrupted by parent cancellation is not
published as a failed check. Cancellation also interrupts interval waiting and
blocked result delivery. Attempt functions must honor context cancellation and
enforce their own timeout, as `Network.Run` does.

The owner runs one scheduler per check and supplies the interval from the shared
effective manifest parameters. Different checks may have independent schedulers.

## Readiness monitoring

`probe.MonitorReadiness` combines the scheduler with the effective readiness
rules for a validated explicit probe. It starts with `Ready: false`, grants
readiness after the configured consecutive successes, and revokes it after the
configured consecutive failures. An opposite result resets the current streak.
Only the initial snapshot and changes are published; `EverReady` stays true
after the first acquisition, including when readiness is later lost.

The runtime adapter supplies the actual process start time. The startup budget
is measured from that fact, so starting the monitor later does not renew it.
The full success threshold must be reached before the startup deadline;
completion at or after it does not grant first readiness. The deadline is
disabled permanently after the first acquisition. A startup failure is terminal
for this monitor: it publishes `StartupFailed`, cancels the active attempt,
waits for cleanup and returns `ErrStartupTimeout`.

Completion time and the pending outcome are recorded together under the same
lock used by the startup deadline. A timely completion is considered even if
its notification is delayed. The scheduler waits for consumption before
recording the next result; a delayed notification cannot apply a result twice.

The supplied attempt receives the resolved probe and a context bounded by the
per-attempt timeout. A callback that returns nil after its context expired still
produces a failed attempt. Parent cancellation or an earlier parent deadline
stops monitoring without declaring startup failure. The active attempt must
honor cancellation; monitoring waits for it to return before exiting.

State observers run serially and must return promptly. Use them to update local
instance facts; outbound reporting must be decoupled so a control-plane outage
cannot block readiness processing or its startup deadline. The monitor only
uses `health.check` and `ready`, leaving restart rules independent.

`MonitorReadinessWithHistory` additionally accepts the owner's previously known
`EverReady` fact for the same process generation. A resumed monitor starts
currently unready with empty streaks and must reacquire the full success
threshold, but a known first readiness permanently disables the startup deadline.
Without that history, the original process start time still bounds startup;
starting a new monitor never renews the budget. The owner must retain the fact
across observation gaps, bind it to the exact generation, and supply it from
trusted Chmura history rather than native readiness flags. This function does not
persist history or resume a generation already declared failed.

## Healthy monitoring

`probe.MonitorHealthy` evaluates an explicit `healthy.check` independently of
readiness. The owner closes its `firstReady` channel once the full readiness
threshold has been acquired, for example from the readiness observer when
`EverReady` first becomes true. No healthy attempt runs before that signal. Once
activated, the check continues even if readiness is later lost.

Each attempt receives its own resolved probe and timeout. Consecutive failures
reach `healthy.unhealthy-failures`; any success resets the streak. Reaching the
threshold terminates the monitor with `ErrUnhealthy`, a decision for the owner
to restart the instance. The monitor performs no restart itself. Parent stop
waits for active cleanup and returns the context cause instead of an unhealthy
decision. Attempts run serially with the same completion-based cadence as the
readiness scheduler.

An absent healthy rule or an omitted `healthy.check` schedules no additional
probe and returns nil. The readiness probe is never inherited. Runtime exit
supervision remains the owner's responsibility, including for process-only
rules and applications with no healthy rule. A restart needs a fresh monitor
and readiness signal for its new process generation.

Process-only readiness without an explicit probe, further runtime operations,
restart actions and agent/engine integration remain separate increments. The
cluster agent will use these components; the CLI continues to talk only to the
control-plane API.

## Readiness publication

`k3s.ReadinessWriter.Apply` publishes one decision to the `chmura/ready` Pod
condition through an explicitly configured client. It requires a complete
container generation, an observed resource version and an idempotency key.
`Resolver.Resolve` and the initial `Observe` target expose their snapshot version.
Only a running Chmura-owned Pod with the declared gate can be changed. Pod or
container replacement, termination and foreign ownership fail before mutation.

The writer updates only its condition through the status subresource. Native
readiness, container status, other conditions and workload configuration remain
untouched. An unchanged readiness value preserves its last transition time.
This adapter neither runs probes nor creates endpoints or routes traffic.

The condition records hashes of the key and versioned intent, never the raw key.
An identical retry can recognize acceptance after a lost response, including
after unrelated kubelet status updates. Reusing the latest key with a different
intent is a conflict. Only the latest receipt is retained; durable operation
history remains a control-plane responsibility.

The base resource version fences the write against intervening updates, even
between its read and write. On conflict the writer reads once to recognize an
identical concurrent request; it never reapplies an old decision against a new
version. An older result cannot overwrite a newer accepted result. The owner
must reconcile the current local readiness decision and runtime snapshot before
issuing a new request with a new key. API and context error causes are retained;
cancellation or transport failure can leave an accepted update.

Publication belongs in a separate reporting worker. Probe observers must return
promptly and perform no outbound I/O. The owner must serialize decisions and own observation
continuity; resource-version checks are not a distributed supervisor lease.
Reconciliation, persistence of `EverReady`, restart supervision and actual agent
wiring remain later increments. Tests use local HTTP API servers, not a live
cluster.
