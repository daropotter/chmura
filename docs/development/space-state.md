# Durable space registry

`state.Space` records a space's opaque, server-assigned ID, installation-wide
name and the ID of exactly one existing cluster. It is control-plane state,
separate from a local project manifest. See the
[domain model](../concepts/domain-model.md#space).

`sqlite.Store.CreateSpace` takes an explicit name, cluster ID and opaque
idempotency key. The caller authorizes space creation and cluster access,
validates the domain name, and resolves any cluster name to a stable ID. The
adapter rejects empty/blank fields and stores the exact supplied strings without
normalization. Each database belongs to one installation.

One atomic insert selects an existing cluster and binds its ID to the new space,
generated space ID and hashed key. An absent cluster returns `state.ErrNotFound`
without creating a cluster/space or reserving the name/key. The foreign key
prevents orphan spaces and deletion of a referenced cluster.

Names are unique across the installation, including across different clusters.
Keys share one space-creation namespace across clusters and callers. An exact
retry returns the same space with `created=false`, including after database
reopen. An accepted key with a different name or cluster ID returns
`state.ErrConflict`, even if the changed cluster ID is missing. A fresh key for
an existing name also conflicts when its requested cluster exists. These
conflicts never change a space's cluster binding. Failed requests do not reserve
otherwise unused names or keys; raw keys are not persisted. Independent handles
rely on SQL uniqueness, not a process mutex.

`GetSpace` reads by stable ID; `GetSpaceByName` resolves an installation-wide
name. Missing reads return `state.ErrNotFound` without creation. The caller must
authorize both reads; the adapter supplies no authentication or RBAC. A canceled
or lost write response can follow commit, so recovery repeats the exact request.
The existing SQLite busy-timeout/cancellation limits still apply.

Schema version 7 adds an initially empty space registry in the transactional
migration chain. Existing cluster identities/creation receipts and operation
history are preserved; migration failure rolls back the chain. There is no
default space. Space creation allocates no project or runtime resources.

This is an internal persistence contract. API/CLI commands, project registration
and deployment remain separate increments. Cluster migration, rename and delete
are not implemented; future changes must preserve stable identity and creation
receipts. Tests use temporary databases, independent handles and frozen
migration fixtures, including cross-cluster name/key races, foreign-key
enforcement, reopen/replay and failure rollback.
