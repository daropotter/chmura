# Manifest reference

!!! note "Draft"
    A per-field reference is being written. The example below is illustrative of
    the current model, not a final schema.

## Naming conventions

Every Chmura-defined key and enum value — in `chmura.yaml` and in the other
config files (`chmura.dev.yaml`, `cluster.yaml`, `installation.yaml`) — is
**kebab-case**: `on-deploy`, `min-age`, `startup-timeout`, `per-instance`,
`multi-attach`. This matches the CLI, whose flags are kebab-case too
(`--fail-on-degraded`), so the same concept is spelled the same in a flag and in
a manifest.

Three things follow their own conventions, not this one:

- **Environment variable names** are `UPPER_SNAKE_CASE` — that is the operating
  system's convention, not Chmura's (`DB_PASSWORD`, not `db-password`).
- **Names you choose** — applications, ports, volumes, endpoints, values — are
  yours; the examples use lowercase, but the rule above is about Chmura's own
  identifiers.
- **External identifiers** — hostnames, image references, file paths — follow
  their own domains.

```yaml
version: 1

name: shop

applications:
  api:
    source:
      image: registry.example.com/shop-api@sha256:...

    instances:
      min: 2
      preferred: 4
      max: 10

    ports:
      http:
        number: 8080
        protocol: http
        visibility: project

    env:
      LOG_LEVEL: info
      DB_PASSWORD:
        secret: db-password
        on-change: restart

    health:
      check:
        http:
          path: /healthz
          port: http
      ready:
        lost-after-failures: 3
      restart:
        after-failures: 6
      startup-timeout: 2m

    mounts:
      uploads:
        volume: uploads
        path: /var/lib/uploads

volumes:
  uploads:
    allocation: shared
    attachment: concurrent
    size:
      min: 70Gi
      preferred: 100Gi
      max: 200Gi
    storage:
      policy: balanced

endpoints:
  api:
    target:
      application: api
      port: http
    listen:
      protocol: https
      port: 443
      hostname: api.shop.example.com
    tls:
      mode: terminate
      certificate: automatic
```
