# Durable project registry

`state.Project` records a stable, opaque server-assigned identity, explicit name
and one existing space ID. Its recognition key is `(space, name)`, as specified
in the [domain model](../concepts/domain-model.md#project). Registration persists
identity separately from the project's manifest and revision history; it creates
no revision or runtime resources.

`sqlite.Store.CreateProject` takes a name, space ID and opaque idempotency key.
The caller authorizes creation in the space, validates the domain name and
resolves any space name to a stable ID. The adapter rejects empty/blank fields
and stores exact supplied strings without normalization. Each database belongs
to one installation.

One atomic `INSERT … SELECT` checks the existing space while binding a generated
project ID, name, space ID and hashed key. Missing spaces return
`state.ErrNotFound` without creating a space/project or reserving a name/key.
The foreign key prevents orphan projects and deletion of a referenced space.

Names and creation keys are scoped to the space, shared by its authorized
callers. Exact retries recover the same record with `created=false`, including
after database reopen. A new key for an existing name or a changed name under an
accepted key returns `state.ErrConflict` without a record. Conflicts do not
rename a project or reserve otherwise unused names/keys. Another space can use
the same name and key to create an independent project with a different ID.
Only key hashes are persisted; independent handles rely on SQL uniqueness.

Both `GetProject(spaceID, id)` and `GetProjectByName(spaceID, name)` require the
already authorized space. Queries filter on that space even for a known project
ID, returning `state.ErrNotFound` for absence or another space's identity. Reads
create nothing. The adapter supplies no authentication or RBAC; callers must
authorize access before querying it. A canceled/lost write response can follow
commit, so recovery repeats the exact request. Existing SQLite busy-timeout
cancellation limits still apply.

Schema version 8 adds an initially empty project registry in the transactional
migration chain. Older cluster/space identities and creation receipts, plus
operation history, are preserved; a failed migration rolls back the chain.
Opening the database creates no default project.

This is an internal persistence contract. Project API/CLI commands, manifest
revision acceptance, deployment and rename/delete remain separate increments.
Future mutations must preserve stable identity and creation receipts. Tests use
temporary databases, independent handles and frozen migration fixtures,
including space-scoped reads/replay, same-name/key independence across spaces,
concurrent creation, foreign-key enforcement and migration preservation/rollback.
