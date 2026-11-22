# Atomic deploy acceptance

`Store.AcceptDeploy` takes an authorized space/project, the expected base
version and revision ID, canonical manifest bytes and an idempotency key. The
caller validates the manifest and project identity before submitting it. The
manifest carries secret references. For a project's first deploy the base is
version zero with an empty revision ID. Later deploys use the pair returned by
`CurrentRevision`.

One SQLite transaction checks project membership and the base pair, creates a
`pending` deploy operation and an immutable snapshot, then records the desired
revision selection at the next version. The operation's private intent names
the created revision. A conflicting base or failed write leaves the previous
selection intact and creates no operation or snapshot. Each fresh accepted
deploy creates a distinct revision, including when manifest bytes are identical.

The project-scoped operation key identifies the original acceptance. A retry
with the same base and bytes returns that operation's current progress and its
original selection, even after the project advances or the database reopens.
Changing the base or bytes under an accepted key returns `ErrConflict`, as does
reusing the key of a standalone operation. Linked snapshot and selection
receipts derive from the operation ID and use a separate prefix. This lets
deploy acceptance share the existing tables with standalone snapshot creation
and revision admission. The client key is stored only as a hash.

Missing or foreign projects return `ErrNotFound`. A lost or canceled reply can
follow commit, so clients retry the exact request. Starting the operation and
tracking rollout progress belong to the execution workflow.
