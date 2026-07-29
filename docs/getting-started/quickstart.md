# Quickstart

!!! info "Design preview"
    Chmura is a design in progress. There is no released build yet — the commands
    on this page describe the intended experience and are how we validate the
    model. They do not run today.

The fastest way to see Chmura work is entirely local. `chmura-dev` stands up a
single-node installation on your machine, builds your app, and deploys it —
using the *same* engine and the *same* deploy path as production. Nothing is
emulated; it is just a small, throwaway installation.

You need `chmura`, `chmura-dev`, and a container runtime (Docker or Podman).

## A tiny app

Any project with a Dockerfile works. Here is a minimal one:

```js title="server.js"
const http = require("http");
http
  .createServer((_, res) => res.end("hello from chmura\n"))
  .listen(8080);
```

```dockerfile title="Dockerfile"
FROM node:20-alpine
WORKDIR /app
COPY server.js .
EXPOSE 8080
CMD ["node", "server.js"]
```

## Describe it

`chmura init` inspects the project and writes a portable manifest,
`chmura.yaml`. It does not connect to anything.

```bash
chmura init
```

```yaml title="chmura.yaml (generated)"
version: 1
name: hello

applications:
  web:
    source:
      context: .
      dockerfile: Dockerfile
    ports:
      http:
        number: 8080
        protocol: http
    health:
      check:
        http:
          path: /
          port: http
```

## Wire up local dev

`chmura-dev init` writes `chmura.dev.yaml` — the dev-only descriptor for values,
seeding, and the watch loop. For an app with no secrets it is nearly empty.

```bash
chmura-dev init
```

## Bring it up

```bash
chmura-dev up
```

This one command:

1. starts a single-node installation in the **dev profile**,
2. creates the `local:dev` space and the project (an explicit bootstrap),
3. builds the image locally and loads it straight into the node — no registry,
4. runs an ordinary deploy.

When it finishes, it prints where the app is reachable:

```text
✓ hello deployed to local:dev

  web   http://localhost:8080   ready (1/1)
```

!!! note "Proposed behavior — under review"
    How the dev profile exposes an application's named ports on `localhost` is a
    design question this quickstart surfaced. The intent: `chmura-dev` forwards
    each named port to a local address and prints it. See the notes at the end
    of this page.

Open <http://localhost:8080> and you should see `hello from chmura`.

## Look around

Everything from here uses ordinary `chmura` commands against `local:dev`, exactly
as you would against any remote:

```bash
chmura status local:dev/
chmura logs   local:dev/ --follow
chmura events local:dev/
```

## Iterate

Run the loop and edit your code. By default each change rebuilds and redeploys
(creating a revision, just like production):

```bash
chmura-dev            # up + deploy + watch + logs
```

If you declare a `reload` command in `chmura.dev.yaml`, source changes are
synced into the running instance without a rebuild — the one place an instance
changes without a new revision, and dev-only by design.

## Tear down

```bash
chmura-dev down                # removes the local installation and its state
chmura-dev down --keep-state   # keep volumes for next time
```

## What this validated — and one open question

Writing this quickstart confirmed the flow end to end: `init` → `dev init` →
`dev up` → observe → iterate. It also surfaced a gap worth deciding explicitly:

- **Reaching the app in dev.** Production reaches an app through an *endpoint*
  (§ networking), but a throwaway local app should be reachable without
  declaring one. The proposal above is that `chmura-dev` auto-forwards named
  ports to `localhost` and prints the URLs. This is a dev-profile convenience,
  not a change to the model — but it needs to be written down as a decision.

Next: **[First steps](first-steps.md)** — the same app, deployed to a real
installation.
