# First steps

!!! info "Design preview"
    Chmura is a design in progress. There is no released build yet — the commands
    on this page describe the intended experience and are how we validate the
    model. They do not run today.

The [Quickstart](quickstart.md) ran everything locally. This page deploys the
same project to a **real installation** — the normal path to staging and
production.

!!! note "You need an installation to deploy to"
    A remote installation, with at least one cluster and node, must already
    exist. Standing one up is an operator task — see
    [Architecture](../architecture.md). If you just want to try Chmura, the
    [Quickstart](quickstart.md) needs none of this.

## 1. Connect to the installation

A **remote** is a named connection to one installation. It does not include a
space — that comes later, in the address.

```bash
chmura remote add company --endpoint https://chmura.company.example
chmura remote login company
```

## 2. Make sure a space exists

A **space** holds your projects. It is never created automatically, and creating
one requires a cluster to place it on:

```bash
chmura space create company:staging --cluster home
```

If you are an application developer, your operator has usually created spaces
for you already. List what you can reach:

```bash
chmura space list company:
```

## 3. Provide the values it needs

The manifest declares *which* values an application needs; the space holds *what
they are*. Set them once — the deploy validates that nothing is missing before
it changes anything.

```bash
chmura var set    company:staging/hello/greeting --value "hello from staging"
chmura secret set company:staging/hello/api-key --file ./secrets/api-key
```

## 4. Deploy

Remember where this project deploys, so you can drop the address afterwards:

```bash
chmura remote use company:staging
```

The first deploy must be told to create the remote project — Chmura never
creates one implicitly:

```bash
chmura deploy --dry-run     # a plan; nothing is applied
chmura deploy --create
```

Later deploys are just:

```bash
chmura deploy
```

## 5. Observe

```bash
chmura status
chmura logs --follow
chmura events
```

Status is the snapshot; logs and events are the time axis.

## 6. Promote to production

The same manifest goes to production — no `if prod` branches. Give production
its own values, then deploy against it explicitly:

```bash
chmura var set    company:production/hello/greeting --value "hello"
chmura secret set company:production/hello/api-key --file ./secrets/prod-api-key

chmura deploy company:production/ --create
chmura status company:production/
```

If a value is missing in production, the deploy stops and lists exactly what to
set — before anything changes. That fail-fast list *is* the promotion checklist.

## The everyday loop

After the first deploy, day-to-day work is small:

```text
edit chmura.yaml   →   chmura deploy   →   chmura status
```

Imperative operations — `chmura app scale`, `chmura app restart`,
`chmura volume resize` — act within the envelopes the manifest declares, without
editing it. Anything outside those envelopes is a manifest change and a deploy.

## Next

- **[Examples](examples.md)** — complete manifests for common shapes.
- **[Domain model](../concepts/domain-model.md)** — the concepts behind remotes,
  spaces, and projects.
