# Design decisions

These are settled choices that shape the rest of the model. Each is a decision
we do not expect to revisit lightly; where the reasoning is subtle, it is
recorded with the decision so the *why* survives.

!!! note "Draft"
    The full log runs to 160+ entries. The entries below are the first batch,
    grouped by area; the rest follow in the same form.

## CLI and UX

1. The CLI is the project's primary UX. The API and web UI are clients over the
   same control plane.
2. Every operation has a non-interactive form. The CLI never starts asking for
   input on its own.
3. Interaction requires `--interactive` / `-i`.
4. `gum` is an optional interactive backend, never a place for business logic.
5. `Ctrl+C` cancels the whole wizard and never triggers a fallback.
6. `--dry-run` always guarantees no state change.
7. There is no `-n` shorthand.
8. `--quiet` suppresses normal stdout but never hides errors on stderr.
9. Help is hierarchical.
10. A confirmation is not data collection: the command prints the full list of
    effects, asks only at a terminal, and without a terminal and without
    `--yes` it errors rather than waiting.

## Addresses and local state

11. There is one address: `[remote:]space/project/application`. There is no
    separate local and remote syntax.
12. The meaning of a partial address follows the command's subject and the
    number of segments, filled from the right.
13. Application operations have their own `app` group — so a project command
    never takes the `project/application` form.
14. A space is always explicit or comes from a project's remembered target. It
    is never guessed.
15. No `default` space. A space is created only by `chmura space create`, with an
    explicit name and cluster.
16. Space names are unique within an installation.
17. Local state is never required for correctness; losing it costs only
    convenience.
18. A project's identity is a server-assigned `project-id`; its recognition key
    is `(space, name)`.
19. Deploy never creates a project or space. Creation is always explicit.

## Values and secrets

20. Values are literals in the manifest, or `var`/`secret` outside it.
21. The manifest declares which values an application needs; the deployment holds
    what they are.
22. `var` and `secret` resolve project-then-space; the manifest never carries a
    scope.
23. A `secret` is writable and never readable. There is no `chmura secret get`.
24. The engine behind secrets is replaceable; the user-facing contract is not.
    The key that encrypts secrets never lives in the state database.

*(Continued — the remaining decisions cover storage, health, rollout,
observability, the admin plane, and local dev.)*
