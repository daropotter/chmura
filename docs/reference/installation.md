# `installation.yaml` reference

`installation.yaml` is the **declared** configuration shared across a whole
installation — the things that must mean the same in every cluster it manages:
storage policies, the certificate issuer, observability retention and sinks, the
identity backend, and the secrets backend.

It is applied with `chmura installation apply` and, like
[`cluster.yaml`](cluster.md), has no per-key create commands — you change the file
and apply it.

```bash
chmura installation apply company: --file installation.yaml --dry-run
chmura installation apply company: --file installation.yaml
```

Why installation-wide and not per-cluster? Because a project asks for a policy
like `fast` by name, and `fast` must guarantee the same thing wherever the
project's space happens to live. If policies were per-cluster, moving a space
between clusters would silently change its guarantees. All keys are kebab-case.

!!! note "Draft"
    The storage-policy model is settled. The issuer, identity, and secrets
    sections have fixed *shapes* but deferred details — noted below.

## Top level

| Key | Type | Purpose |
| --- | --- | --- |
| `version` | integer | Schema version. Currently `1`. |
| `storage-policies` | map | Portable storage requirements, keyed by name. |
| `certificates` | object | The issuer behind `certificate: automatic`. |
| `observability` | object | Built-in retention and optional external sinks. |
| `identity` | object | How users authenticate. |
| `secrets` | object | The backend that stores secrets. |

## `storage-policies.<name>`

A policy is a portable set of tag requirements. A project names a policy; Chmura
matches it to a cluster [pool](cluster.md#storage-poolsname) whose tags satisfy
it. The name is all a project ever uses — never a pool.

```yaml
storage-policies:
  fast:
    tags:
      required:  [ssd]
      preferred: [nvme]
      forbidden: [experimental]

  balanced:
    tags:
      required:  [backup]
      preferred: [ssd]

  cheap:
    tags:
      required:  [backup]
      preferred: [hdd]
```

| Key | Type | Meaning |
| --- | --- | --- |
| `tags.required` | list | A pool must carry **all** of these, or it does not match. |
| `tags.preferred` | list | Missing ones allow degradation, never rejection. |
| `tags.forbidden` | list | A pool carrying **any** of these is rejected. |

A cluster with no pool satisfying a policy is not a configuration error — it is a
cluster where projects requesting that policy will not fit, which `chmura doctor`
reports.

Fallback is not declared here: a *project* may list fallback policies in its
manifest (`storage.fallback`). A policy definition is only its tags.

### How a project composes with a policy

A project references a policy and may add its own tag requirements. The rule:

> A project may **tighten** a policy's requirements. It may never relax them.

```text
required   union of the policy's and the project's
forbidden  union of the policy's and the project's
preferred  union of both, with no effect on admissibility
```

A project cannot drop a tag from a policy's `required`, nor lift a `forbidden` —
otherwise the policy would stop being an administrator's guarantee and become a
suggestion. An internal contradiction — the same tag `required` by the policy and
`forbidden` by the project — is a validation error, not a silent resolution in
anyone's favor.

## `certificates`

The issuer behind `certificate: automatic` on an endpoint (see
[Networking](../networking.md#certificates)). It is installation-wide because a
certificate for `shop.example.com` is issued the same way regardless of which
space or cluster serves it.

```yaml
certificates:
  issuer:
    acme:
      directory: https://acme-v02.api.letsencrypt.org/directory
      contact: ops@example.com
      challenge: http-01        # or dns-01
```

| Key | Type | Purpose |
| --- | --- | --- |
| `issuer.acme.directory` | url | The ACME directory endpoint. |
| `issuer.acme.contact` | string | Contact address for the account. |
| `issuer.acme.challenge` | enum | `http-01` or `dns-01`. |

!!! note "Deferred"
    The full issuer contract — challenge-specific settings (DNS provider
    credentials for `dns-01`), account key handling, and support for non-ACME
    issuers — is being settled. For a homelab behind NAT where port 80 is not
    reachable, `http-01` will not work and `dns-01` or a provided certificate is
    the path.

## `observability`

The built-in store's retention window, and the optional external sinks that
receive forwarded signals. See [Observability](../observability.md).

```yaml
observability:
  retention:                 # built-in, bounded
    events:  30d
    logs:    3d
    metrics: 24h

  forward:                   # optional external sinks
    metrics: { otlp: https://collector.example/v1/metrics }
    logs:    { otlp: https://collector.example/v1/logs }
    traces:  { otlp: https://collector.example/v1/traces }
```

| Key | Type | Purpose |
| --- | --- | --- |
| `retention.events` / `logs` / `metrics` | duration | How long the bounded built-in store keeps each signal. |
| `forward.metrics` / `logs` / `traces` | object | An external sink (`otlp` endpoint) to forward to. Omitted means only the built-in window exists. |

With no `forward`, what falls out of the retention window is gone — and that is
stated plainly, never hidden behind the pretense of unlimited history.

## `identity`

How users authenticate to the control plane behind `chmura remote login`. The
interface is fixed; the backend is replaceable — built-in accounts for a homelab,
or delegation to an external OIDC provider for an organization. See
[Architecture](../architecture.md).

```yaml
identity:
  backend: builtin            # or: oidc
  # oidc:
  #   issuer:    https://id.example.com
  #   client-id: chmura
```

| Key | Type | Purpose |
| --- | --- | --- |
| `backend` | enum | `builtin` (the installation's own accounts) or `oidc` (delegate to a provider). |
| `oidc.issuer` | url | OIDC issuer, when `backend: oidc`. |
| `oidc.client-id` | string | Registered client ID. |

!!! note "Deferred"
    The full OIDC settings (scopes, claim mapping, group-to-role binding) belong
    with RBAC, which is deferred. The first admin account is created by
    `chmura-server init`, not here.

## `secrets`

The backend that stores secret values. Chmura does not build its own secret
manager; it defines the interface and a minimal built-in, with external drivers
as a replacement. See [values and secrets](../getting-started/first-steps.md).

```yaml
secrets:
  backend:
    driver: builtin           # or an external driver
```

| Key | Type | Purpose |
| --- | --- | --- |
| `backend.driver` | enum | `builtin` (envelope encryption on a proven library, no extra infrastructure) or an external driver. |

Regardless of driver, the key that encrypts secrets never lives in the state
database — otherwise a routine state backup would be a complete secret leak.

!!! note "Deferred"
    The set of external drivers (Vault, OpenBao, …) and where the encrypting key
    is stored are being settled. The `driver` field exists from the start so
    adding one is not a breaking change.

## Complete example

```yaml
version: 1

storage-policies:
  fast:
    tags: { required: [ssd], preferred: [nvme], forbidden: [experimental] }
  balanced:
    tags: { required: [backup], preferred: [ssd] }

certificates:
  issuer:
    acme:
      directory: https://acme-v02.api.letsencrypt.org/directory
      contact: ops@example.com
      challenge: http-01

observability:
  retention: { events: 30d, logs: 3d, metrics: 24h }
  forward:
    metrics: { otlp: https://collector.example/v1/metrics }

identity:
  backend: builtin

secrets:
  backend: { driver: builtin }
```

Storage **pools** — the physical resources these policies match against — are not
here. They are per-cluster, in [`cluster.yaml`](cluster.md).
