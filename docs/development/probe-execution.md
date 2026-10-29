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

The supplied attempt receives the resolved probe and a context bounded by the
per-attempt timeout. A callback that returns nil after its context expired still
produces a failed attempt. Parent cancellation or an earlier parent deadline
stops monitoring without declaring startup failure. The active attempt must
honor cancellation; monitoring waits for it to return before exiting.

State observers run serially and must return promptly. Use them to update local
instance facts; outbound reporting must be decoupled so a control-plane outage
cannot block readiness processing or its startup deadline. The monitor only
uses `health.check` and `ready`, leaving restart rules independent.

Process-only readiness without an explicit probe, process/exec execution,
restart decisions/actions and agent/engine integration remain separate
increments. The cluster agent will use these components; the CLI continues to
talk only to the control-plane API.
