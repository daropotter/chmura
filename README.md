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

Design specification, work in progress. The model is settled across the areas
documented here; implementation has not started. Nothing in these docs is a
promise of a shipped feature.

## Documentation

The docs live in [`docs/`](docs/) and are written as plain Markdown. They render
as-is on GitHub Pages and are also set up to build with
[Material for MkDocs](https://squidfunk.github.io/mkdocs-material/).

```bash
pip install mkdocs-material
mkdocs serve          # live preview at http://localhost:8000
mkdocs gh-deploy      # publish to GitHub Pages
```

Start at [`docs/index.md`](docs/index.md).

## Repository layout

```text
docs/           documentation (Markdown)
  concepts/     the mental model — what the pieces are
  reference/    manifest schema and worked examples
mkdocs.yml      docs-site configuration
```
