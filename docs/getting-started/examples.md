# Examples

!!! info "Design preview"
    Chmura is a design in progress. There is no released build yet — these
    manifests illustrate the intended model, not a final schema.

Small, complete manifests for common shapes. Each is a full `chmura.yaml` you
could deploy as-is once a target space exists.

## A stateless web service

One application, one HTTP port, served publicly over HTTPS with an
automatically-managed certificate.

```yaml
version: 1
name: site

applications:
  web:
    source:
      image: registry.example.com/site@sha256:...
    instances:
      min: 2
      preferred: 3
      max: 6
    ports:
      http:
        number: 3000
        protocol: http
    health:
      check:
        http:
          path: /healthz
          port: http

endpoints:
  public:
    target:
      application: web
      port: http
    listen:
      protocol: https
      port: 443
      hostname: example.com
    tls:
      mode: terminate
      certificate: automatic
```

## A web app with a background worker

Two applications in one project. `chmura deploy` ships the whole project;
`chmura app restart worker` acts on one.

```yaml
version: 1
name: shop

applications:
  api:
    source:
      image: registry.example.com/shop-api@sha256:...
    instances:
      min: 2
      preferred: 2
      max: 8
    ports:
      http:
        number: 8080
        protocol: http
    env:
      DB_PASSWORD:
        secret: db-password
        onChange: restart
    health:
      check:
        http:
          path: /healthz
          port: http

  worker:
    source:
      image: registry.example.com/shop-worker@sha256:...
    instances:
      min: 1
      preferred: 2
      max: 4
    # no port and no health check: readiness is process-only, shown plainly

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

## A stateful service with per-instance storage

Each instance gets its own volume, bound to its stable slot. The size is
per instance, never a shared total.

```yaml
version: 1
name: db

applications:
  node:
    source:
      image: registry.example.com/db@sha256:...
    instances:
      min: 3
      preferred: 3
      max: 3
    ports:
      sql:
        number: 5432
        protocol: tcp
        visibility: project
    mounts:
      data:
        volume: data
        path: /var/lib/data
    deploy:
      strategy:
        replace: swap    # exclusive volume: one runtime at a time
        floor: 2         # keep 2 of 3 ready during a rollout

volumes:
  data:
    allocation: per-instance
    attachment: exclusive
    size:
      min: 20Gi
      preferred: 50Gi
      max: 200Gi
    storage:
      policy: fast
      tags:
        required: [ssd]
    lifecycle:
      detached:
        policy: retain
```

## Notes

- **Values** referenced as `secret:` or `var:` must exist in the target space
  before deploy — the deploy fails fast and lists any that are missing.
- **Where it deploys** is never in the manifest. The same file goes to staging
  and production, addressed explicitly or through a remembered target.
- See the [manifest reference](../reference/manifest.md) for the full field set.
