# CLI contract

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

## UX and DX principles

Non-interactive by default; explicit interaction; reproducible wizards; layered
help; the `stdout`/`stderr` contract; confirmations that state their effect.

## Global flags

`--dry-run`, `--quiet`, `--verbose`, `--interactive`, `--output`, `--file`/`-C`,
`--yes`, `--fail-on-degraded`, and exit codes.

## Addresses

The one address grammar — `[remote:]space/project/application` — and how meaning
follows the command's subject, with segments filled from the right. Remotes,
explicit spaces, local paths, and the remote-selection order.

## init and project files

`chmura init`, `chmura.yaml`, `chmura.local.yaml`, precedence, the user config,
and the local project registry.

## Command groups

High-level shortcuts, spaces, config values, projects, applications, volumes,
the admin plane, remotes, operations, observability, and diagnostics.
