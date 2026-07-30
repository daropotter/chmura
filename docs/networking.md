# Networking & endpoints

An application declares the ports it listens on. Who can reach each port, and
whether it is exposed to the outside world, is layered on top — first by
**visibility**, then by an explicit **endpoint**. Nothing is reachable that you
did not declare.

## Named ports

An application can have several ports, each with a name, a number, a protocol,
and a visibility.

```yaml
applications:
  calls:
    ports:
      http:
        number: 8080
        protocol: http
        visibility: project
      signaling:
        number: 8443
        protocol: https
        visibility: project
      turn:
        number: 3478
        protocol: udp
        visibility: space
```

Multiple ports is not an error. The only error is when Chmura must pick a port
automatically and the choice is ambiguous.

Everything downstream refers to a port **by name**, never by number. Changing
`8080` to `9000` in the application touches nothing else.

### Protocol

`protocol` says *what the stream is*, not how advanced it is:

| `protocol` | Layer | Meaning |
| --- | --- | --- |
| `http` | L7 | the app speaks HTTP, no TLS |
| `https` | L7 | the app speaks HTTP and terminates TLS itself |
| `tcp` | L4 | an opaque byte stream |
| `udp` | L4 | datagrams |

**L7** (layer 7) means application-aware: Chmura can read the HTTP inside the
stream — hostnames, headers — and route on them. **L4** (layer 4) means a raw
transport stream that Chmura forwards without understanding its contents.

The L7/L4 distinction is not cosmetic. It decides whether a port can be
multiplexed by hostname — and therefore whether an endpoint over it can share a
listener with other projects. See [uniqueness](#uniqueness-and-conflicts).

## Visibility

Visibility controls who may reach a named port.

| Level | Reachable by |
| --- | --- |
| `application` | runtimes of the same application |
| `project` | every application in the project |
| `space` | every project in the space |
| `public` | traffic from outside Chmura, through an endpoint |

The default for a named port is `project`.

## Endpoints

An **endpoint** exposes a named port to the outside world. It maps an external
listener onto an application port — by name.

```yaml
applications:
  api:
    ports:
      http:
        number: 8080
        protocol: http
      admin:
        number: 8081
        protocol: http
        visibility: application

endpoints:
  website:
    target:
      application: api
      port: http
    listen:
      protocol: https
      port: 443
      hostname: api.example.com
    tls:
      mode: terminate
      certificate: automatic
```

This maps:

```text
https/443  api.example.com  →  api:http  →  http/8080
```

The application's port number never appears in the endpoint. A second endpoint
of the same application, on a different port and hostname, coexists on port 443:

```yaml
endpoints:
  admin:
    target:
      application: api
      port: admin
    listen:
      protocol: https
      port: 443
      hostname: admin.api.example.com
    tls:
      mode: terminate
      certificate: automatic
```

Both listen on 443 and do not collide, because they differ by hostname. Why that
is allowed is the subject of [uniqueness](#uniqueness-and-conflicts).

### Values that vary by environment

The same manifest deploys to staging and production, so a hostname that differs
between them should not be hard-coded. An endpoint hostname may reference a
`var`, resolved from the target space exactly like an environment value —
project-then-space, with the same fail-fast check and visible provenance:

```yaml
listen:
  protocol: https
  port: 443
  hostname:
    var: public-hostname
```

Only `var` is allowed here, never `secret`: a hostname is not a secret, and a
manifest field is resolved at *plan* time to build the deployed spec, not
injected into the app at runtime. Chmura deliberately stops short of full
templating — the manifest stays something you can read and understand without a
values file, and only genuinely deployment-varying identifiers (a hostname
today) accept a reference.

## Ingress

An endpoint is a project resource, but the port it listens on is shared. The
object that represents that shared surface is an **ingress** — a named entry
point for traffic into a cluster, declared by an administrator in `cluster.yaml`
(see [Architecture](architecture.md)).

```yaml
# cluster.yaml
ingress:
  default:
    addresses:
      - 203.0.113.10
  internal:
    addresses:
      - 10.0.0.5
    spaces:
      - production
```

- `addresses` — where traffic actually arrives.
- `spaces` — optional restriction on which spaces may use this ingress. Omitted
  means all spaces in the cluster.

An endpoint selects an ingress by name, defaulting to `default`:

```yaml
endpoints:
  internal-api:
    ingress: internal
    target:
      application: api
      port: http
    listen:
      protocol: https
      port: 443
      hostname: api.internal
    tls:
      mode: terminate
      certificate: automatic
```

### DNS stays outside

Chmura does not manage DNS. Pointing `api.example.com` at the ingress address is
done outside Chmura — by hand, or by Terraform acting as an adapter.

`chmura doctor` does check that a hostname declared in an endpoint actually
resolves to its ingress — one of those mistakes that otherwise only surfaces
under live traffic:

```text
! Endpoint "website" listens on api.example.com, but that name resolves to
  198.51.100.7, not to ingress "default" (203.0.113.10).
```

## Uniqueness and conflicts

This is the heart of the topic. An external port is either a shared resource or
an exclusive one, and which it is depends on the protocol.

| Protocol | Listener | Discriminator |
| --- | --- | --- |
| `http`, `https` | multiplexed | hostname — many endpoints, even across projects, share one port |
| `tcp`, `udp` | exclusive | none — one endpoint owns the whole port |

So the conflict key is:

```text
http / https   (ingress, port, hostname)
tcp / udp      (ingress, port)
```

### Conflicts are global within an ingress

The key contains neither the project nor the space. If two spaces share an
ingress, `api.example.com` can exist in only one of them — which is correct,
because it is one DNS name pointing at one address. Separate namespaces need
separate ingresses.

The plan detects the conflict before anything changes:

```text
Error: endpoint "website" cannot claim api.example.com:443 on ingress "default".

  already claimed by: project shop in space company:staging

  use a different hostname, or release the claim in the other project
```

This deliberately reveals that a project exists in another space. The conflict is
real, and staying silent about its cause would be worse — the plan would have to
refuse without a reason. Hiding it is a job for access control, not for the
conflict message.

A claim lasts as long as the deployed endpoint. Removing the project or the
endpoint from the manifest releases it on the next deploy.

### Wildcards

A hostname may be a wildcard:

```text
*.example.com
```

An exact match wins over a wildcard. Two identical wildcards conflict just like
two identical names.

### No path routing, no catch-all

There is no URL-path routing in the first version. The conflict key is complete —
`(ingress, port, hostname)` — and cannot be narrowed by `/api` vs `/www`.

There is also no hostname-less endpoint acting as a default backend. A catch-all
captures traffic no one consciously decided on, which runs against the rest of
the model.

## TLS

A `tls` block appears **only** for `listen.protocol: https`, and settles one
question: who ends the encryption.

| `tls.mode` | Meaning |
| --- | --- |
| `terminate` | Chmura decrypts the traffic and holds the certificate |
| `passthrough` | Chmura does not decrypt; the application holds the certificate |

These are the standard load-balancer terms *TLS termination* and *TLS
passthrough*. With `terminate`, the encryption ends at Chmura's edge — it opens
the traffic, so it needs the certificate. With `passthrough`, Chmura relays the
sealed bytes untouched, and only the application can open them.

`http` has no `tls` block — it is explicitly unencrypted. `tcp` and `udp` have
none either: on an exclusive L4 port, any TLS is entirely the application's
business and Chmura never sees it.

The full picture, including how Chmura finds the backend:

| `listen.protocol` | TLS at the edge | Certificate | Backend chosen by | Port |
| --- | --- | --- | --- | --- |
| `http` | none | — | Host header | shared |
| `https` + `terminate` | ends at Chmura | Chmura | Host, after decrypt | shared |
| `https` + `passthrough` | ends at the app | application | SNI, no decrypt | shared |
| `tcp` | invisible to Chmura | application | none — one endpoint | exclusive |
| `udp` | — | — | none — one endpoint | exclusive |

Two things this table answers at once:

**Who holds the certificate.** Only `terminate` puts the certificate under
Chmura's management. With `passthrough` and at L4, the certificate — if there is
one — belongs to the application.

**How Chmura picks the endpoint.** To match `(ingress, port, hostname)` it must
see the hostname: from the `Host` header (plaintext), or from the SNI in the TLS
handshake — SNI is visible before anything is decrypted. That is why
`https + passthrough` can share a port despite being encrypted: SNI is enough to
route. `tcp`/`udp` carry no hostname, so there is nothing to distinguish and the
port is exclusive.

After `terminate`, Chmura connects to the instance according to the *target*
port's protocol: `http` in the clear, `https` re-encrypted. There is no separate
field — the port declaration already says so.

Any combination outside the table is a validation error.

## Certificates

`terminate` needs a certificate. There are two sources.

```yaml
tls:
  mode: terminate
  certificate: automatic
```

```yaml
tls:
  mode: terminate
  certificate:
    secret: api-tls-cert
```

`automatic` means a certificate issued by the installation's configured issuer.
Issuer configuration — ACME directory, contact, how domain control is proven —
lives in `installation.yaml`, because it is shared across every space and
project.

The secret variant uses the ordinary [secret](getting-started/first-steps.md)
mechanism with no exceptions: resolved project-then-space, with the same
existence check at deploy time.

### How a request flows

The declarations above describe intent. Here is what happens when a request
arrives. Traffic is handled by the **edge proxy** — the reverse proxy at the cluster
boundary that terminates TLS, routes by host or SNI, and load-balances across
ready instances. It is realized by the execution engine and invisible in the
user model. For an HTTPS request with `terminate`:

```text
1. The client connects to the ingress address on the endpoint's port (e.g. 443).

2. The edge layer reads the hostname:
     http  → from the Host header
     https → from SNI, before any decryption

3. It matches (ingress, port, hostname) to an endpoint.
     no match → the connection is refused; there is no catch-all

4. terminate:   decrypts with the endpoint's certificate
   passthrough: forwards the bytes without decrypting

5. It load-balances across the target application's ready instances,
   skipping the not-ready ones (see below).

6. It connects to the instance using the target port's protocol.
```

Step 5 is where endpoints meet readiness: a not-ready instance receives no public
traffic, exactly as it does not count toward rollout progress. It is the same
readiness state, one mechanism for both — see [Deployment](deployment.md).

### Renewal never touches the application

For both sources, the certificate is held by the edge proxy, not the instance.
So renewal — automatic, or by replacing the secret — is not a manifest change:
it creates no revision and restarts nothing.

## Service discovery and load balancing

The sections above leave two questions open: how does one application reach
another *inside* Chmura, and how is traffic spread across an application's
instances? Both are answered by built-in machinery — you do not stand up a
separate service registry or load balancer.

### Internal DNS

Every application is reachable inside Chmura by a stable name, without declaring
anything. The name resolves within the scope its port's visibility allows:

```text
api           the api application, from within the same project
api.shop      the api application, from elsewhere in the space
```

A bare name load-balances across the application's *ready* instances; you connect
on the port number you need. This internal DNS is entirely separate from the
external DNS above — Chmura generates it, scopes it by visibility, and it never
leaves the installation.

For workloads that must address a *specific* instance — a database replica, a
leader — each stable slot has its own name:

```text
api-0, api-1, api-2      a specific slot, always the same instance behind it
```

Per-slot names are what make per-instance volumes useful: `api-0` always resolves
to the instance that owns `data/0`, across restarts and deploys.

### Load balancing

Balancing traffic across ready instances is built in and always on. It is not an
optional add-on or an external component — it is *how* both endpoints
(north–south) and internal names (east–west) reach an application at all.

- **Health-aware.** Only ready instances receive traffic. A not-ready or failing
  instance is taken out and put back automatically, using the same readiness
  signal as rollout — see [Deployment](deployment.md).
- **Round-robin by default**, across the ready set.
- **Session affinity** is opt-in per endpoint, for workloads that keep
  per-client state:

    ```yaml
    endpoints:
      website:
        # ...
        affinity: client-ip     # or: cookie
    ```

### Never route to zero: the panic threshold

Health-aware balancing has a failure mode of its own. When instances share a
downstream dependency and that dependency degrades, *every* instance can lose
readiness at once. Naïvely, the balancer would pull them all from rotation — and
now there is nowhere to send traffic. Worse, an autoscaler adding instances only
feeds the fire, because the new ones fail the same check, and a rollout gated on
readiness thrashes instead of waiting for the dependency to recover.

Chmura's load balancer does not fall into this. Below a **panic threshold** of
ready instances (a configurable fraction, sensible default), it stops honoring
readiness and routes to *all* instances:

```text
enough ready      route to the ready set only
below threshold   assume the health signal is systemic, not per-instance —
                  route to everyone rather than to no one
```

The reasoning: when most of a fleet reports unready simultaneously, the likeliest
cause is a shared dependency or a check that is lying, not that every instance is
individually broken. Pulling them all out makes the outage total; keeping them in
lets requests through to degrade rather than fail outright.

This pairs with a guideline the [health model](deployment.md#restart-is-a-deliberate-choice)
already states from the other side: a `restart` rule must depend only on the
process, never a shared dependency, so a downstream blip never triggers a restart
storm. Readiness may reflect a dependency; the panic threshold is what keeps that
from taking the whole fleet down.

### Deferred

Weighted or canary traffic splitting, retries, circuit breaking, and outlier
ejection live in the same edge proxy as a future capability. Canary in particular
pairs with a future deploy strategy that shifts a share of traffic between
revisions. Until then, traffic is spread evenly across the ready set.

## Firewall and egress

Inbound rules are derived from named ports, visibility, and endpoints. Inbound
traffic is denied by default, except along explicitly declared paths.

Outbound traffic is a separate policy:

```yaml
applications:
  api:
    network:
      egress:
        internet: true
```

The initial default is `internet egress: allowed`. It can be turned off
explicitly:

```yaml
network:
  egress:
    internet: false
```

More precise per-domain allowlists may be added later.
