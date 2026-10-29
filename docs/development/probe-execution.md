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

Only one attempt is performed per invocation. Per-check scheduling and overlap
prevention, consecutive-outcome thresholds, startup readiness, process/exec
probes, restart actions and agent/engine integration remain separate increments.
The cluster agent will use this component; the CLI continues to talk only to the
control-plane API.
