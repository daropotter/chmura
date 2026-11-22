# Immutable manifest snapshots

`state.Revision` retains an opaque server-assigned ID, an existing project ID
and immutable manifest bytes. It supplies storage for the
[revision model](../deployment.md#local-manifest-vs-remote-state). The adapter
inserts and reads snapshots; project activation and deploy admission are separate
contracts. A stored revision carries no rollout outcome or execution permission.

`sqlite.Store.CreateRevision` takes the authorized space/project IDs, an opaque
idempotency key and nonempty canonical manifest bytes. The caller checks project
access, validates the manifest and its project identity, and produces the
canonical encoding. Payloads contain secret references, never secret values.
The adapter rejects blank scope/key fields and empty bytes; it stores the exact
bytes without parsing, normalization or engine translation. There is no public
manifest wire-format decision in this storage contract.

Parent selection and insertion are one atomic statement. A missing project, or
a project outside the supplied space, returns `state.ErrNotFound` without a
revision or a reserved key. No parent or default revision is created. The
foreign key prevents orphan revisions and deletion of a referenced project.

Revision-creation keys are scoped to the project, shared by its authorized
callers. Exact retries recover the original ID and bytes with `created=false`,
including after database reopen or later snapshots. Changed bytes under an
accepted key return `state.ErrConflict` with no record and preserve the original
snapshot. A new key creates a new revision even for identical bytes: each deploy
can retain its own immutable revision, including failed rollout attempts.
Independent projects can reuse the same key. Only key hashes are persisted;
uniqueness in the database serializes independent handles.

`GetRevision(spaceID, projectID, id)` checks both project and space via the
project relationship before returning bytes. Missing or foreign IDs return
`state.ErrNotFound` and a zero record. The caller authorizes access; this adapter
provides no authentication or RBAC. Returned bytes are independent of stored
data, as are request buffers after a completed call. Lost/canceled write replies
can follow commit; retry the exact request. Existing SQLite busy-timeout
cancellation limits still apply.

Schema version 9 adds an initially empty revision table through the transactional
migration chain. Existing project identities/creation receipts and operation
history are preserved; failure rolls back the chain. Tests cover retained byte
ownership, reopen/history replay, unchanged-content requests, scoped reads and
creation, concurrency, foreign keys, storage failures and frozen migration
fixtures, using temporary databases only.

Snapshot storage leaves the project's selection unchanged.
[Revision admission](revision-admission.md) separately checks a base version/ID
pair and binds a desired selection to an existing running operation. The public
API/CLI acceptance workflow and rollout remain separate increments. Snapshot
IDs have no ordering guarantee; `CurrentRevision` supplies the latest admitted
desired selection, separately from observed rollout state.
