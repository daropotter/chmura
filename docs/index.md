# Chmura

Chmura is an open-source platform for running a private cloud and deploying
applications to it — on physical or virtual machines you control.

The primary interface is a command-line tool. A web UI and an API are additional
clients over the same control plane; they are never separate sources of logic.
Kubernetes can serve as the execution engine underneath, but its objects and
terminology are never required of you.

!!! quote
    Infrastructure should feel like deploying an application.

## What Chmura gives you

Chmura hides the complexity of infrastructure without taking away your ability
to drop down a level. You reason in a small, learnable vocabulary:

| You think in terms of | …instead of |
| --- | --- |
| installations, remotes, clusters | control planes, API endpoints, kubeconfigs |
| spaces, projects, applications | namespaces, deployments, pods |
| volumes, endpoints, storage policies | PVCs, ingresses, storage classes |

You never have to name an object of the underlying orchestrator to get work
done. When you *do* want to go deeper, the lower levels are still there.

## CLI-first

The command line is the core experience. Everything else is a client over the
same control plane.

```bash
chmura init
chmura deploy
chmura status
chmura logs
chmura app restart api
chmura doctor
```

The API and web UI are alternative front-ends over the same model — not
alternative implementations of the logic.

## The shape of a workflow

A typical project lives in a Git repository with a single manifest,
`chmura.yaml`, that describes the desired state. Where it deploys is not part of
the manifest — that comes from a **remote** and a **space** you address
explicitly.

```bash
git clone https://example.com/shop.git
cd shop

chmura init                       # detect the project, write chmura.yaml
chmura remote use company:staging # remember where this project deploys

chmura deploy --dry-run           # a plan, with no changes applied
chmura deploy --create            # first deploy creates the remote project

chmura status
chmura logs
chmura deploy company:production/ --create
```

## Principles

Chmura is opinionated, and the opinions are consistent across every feature:

- **Non-interactive by default.** Commands never start asking questions on their
  own. Interaction is explicit (`--interactive`), and every wizard prints the
  equivalent non-interactive command.
- **Nothing is created silently.** Spaces, projects, and configuration values
  never spring into existence on first use. Missing things are a fast, precise
  error — not a guess.
- **Degradation over binary failure.** Where you declare minimums and fallbacks,
  Chmura runs degraded and says so, rather than refusing outright.
- **A weak guarantee never looks like a strong one.** Best-effort behavior,
  missing probes, and bounded history are stated plainly, everywhere state is
  shown.
- **Local state is never required for correctness.** A fresh clone, or CI with
  no local state at all, behaves correctly. Losing local state costs
  convenience, never safety.

## Where to go next

<div class="grid cards" markdown>

- **[Domain model](concepts/domain-model.md)** — the pieces and how they fit.
- **[CLI contract](cli.md)** — addresses, flags, output, exit codes.
- **[Networking & endpoints](networking.md)** — ports, endpoints, TLS, routing.
- **[Deployment & rollout](deployment.md)** — health, strategies, revisions.
- **[Observability](observability.md)** — logs, events, metrics, traces.
- **[Architecture](architecture.md)** — control plane, agents, admin plane.
- **[Local dev](local-dev.md)** — the same engine, in a dev profile.
- **[Design decisions](decisions.md)** — the settled choices, with rationale.

</div>
