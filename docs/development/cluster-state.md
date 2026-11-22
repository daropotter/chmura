# Durable cluster registry

`state.Cluster` records an explicit installation cluster's opaque, server-assigned
ID and name. A cluster starts empty, as specified in the
[domain model](../concepts/domain-model.md#cluster). Its registration does not
configure an engine, enroll nodes or allocate runtime resources.

`sqlite.Store.CreateCluster` takes a name and an opaque idempotency key. The
caller authorizes installation administration and validates the domain name;
the adapter rejects empty/blank fields and stores the exact supplied name. It
does not infer, trim or normalize names. Each database belongs to one installation.

Names are unique across that installation. The key namespace is cluster creation
within the installation, shared by all callers. One atomic insert binds the
hashed key, name and generated ID. The raw key is never stored. Retrying the same
request returns the original cluster and `created=false`, including after
reopening the database. Reusing the key for another name or requesting an
existing name with another key returns `state.ErrConflict` without a record.
Failed creation does not reserve an otherwise unused name or key. Independent
database handles rely on SQL uniqueness, not a process-local mutex.

`GetCluster` resolves a stable ID; `GetClusterByName` resolves an
installation-wide name. Missing records return `state.ErrNotFound` and create
nothing. Both reads require the caller to authorize installation inventory
access; this adapter provides no authentication or RBAC. A canceled or lost
write response can follow a commit, so recovery repeats the exact creation
request. Existing SQLite busy-timeout/cancellation limits still apply.

Schema version 6 adds the initially empty cluster registry in the same
transactional migration chain as operation/process history. It preserves older
records and rolls back the chain on failure. Opening the database never creates
a default cluster. The registry currently has no rename or delete method; those
future changes must preserve creation receipts and stable identity.

This is an internal persistence contract. Cluster API/CLI commands, spaces,
projects, engine configuration and enrollment remain separate increments. Tests
use temporary databases, independent handles and frozen migration fixtures,
including terminal history preservation and migration-chain rollback.
