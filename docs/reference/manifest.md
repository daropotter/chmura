# Manifest reference

!!! note "Draft"
    A per-field reference is being written. The example below is illustrative of
    the current model, not a final schema.

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
        onChange: restart

    health:
      check:
        http:
          path: /healthz
          port: http
      ready:
        lostAfterFailures: 3
      restart:
        afterFailures: 6
      startupTimeout: 2m

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
