# Chmura

**Chmura** is an open-source platform for running a private cloud and deploying
applications to it — on physical or virtual machines you control. The primary
interface is a command-line tool; a web UI and API are additional clients over
the same control plane, never separate sources of logic.

> Infrastructure should feel like deploying an application.

Kubernetes can serve as the execution engine underneath, but you never have to
learn or touch its objects. You think in terms of installations, remotes,
clusters, spaces, projects, applications, volumes, and endpoints — and nothing
lower unless you choose to.

## Status

The design specification is settled and
[published](https://daropotter.github.io/chmura/); it will keep evolving
alongside the project. Implementation has just begun and is at an early stage —
nothing here is a promise of a shipped feature.

## Documentation

The docs live in [`docs/`](docs/) and are published at
**<https://daropotter.github.io/chmura/>**. They are written as plain Markdown,
render as-is on GitHub Pages, and also build with
[Material for MkDocs](https://squidfunk.github.io/mkdocs-material/).

```bash
pip install mkdocs-material
mkdocs serve          # live preview at http://localhost:8000
mkdocs gh-deploy      # publish to GitHub Pages
```

Start at [`docs/index.md`](docs/index.md).

## Repository layout

```text
cmd/            binary entrypoints (chmura, chmura-server, chmura-agent, chmura-dev)
internal/       implementation
docs/           documentation (Markdown)
  concepts/     the mental model — what the pieces are
  reference/    manifest schema and worked examples
  development/  stack, repo layout, engineering decisions
mkdocs.yml      docs-site configuration
```

## Contributing

Chmura is in early implementation. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
workflow, and [`AGENTS.md`](AGENTS.md) for the shared context AI agents and humans
work from.
