# Durable operation state

`internal/state` defines portable submission and operation records.
`internal/state/sqlite` is the initial single-mode persistence adapter. It is a
library; the server does not open it or expose operation endpoints yet.

`Open` requires an explicit absolute disk path. It creates the database/schema,
but no directories, spaces, projects or other domain objects. Paths are encoded
as file URIs so literal `?`/`#` characters cannot select an in-memory database or
inject connection options. The caller owns file permissions and backup policy.
This is the control-plane database, never correctness state on a CLI machine.

The pure-Go `modernc.org/sqlite` driver preserves the CGO-free release model.
Version 1.59.0 supports the existing Go 1.25 floor; later minor versions require
Go 1.26. Connections use FULL synchronization, foreign keys and a five-second
busy timeout. Opening does not change the persistent journal mode; new databases
use SQLite's rollback journal. Setting WAL during concurrent first opens can
return SQLITE_BUSY even with a busy timeout, so any future journal-mode change
needs an explicitly coordinated migration. Schema initialization is transactional
with an immediate write lock and a version check; unsupported versions are
rejected. Schema version 2 upgrades version-1 acceptance records in the same
transaction, retaining identity, key bindings and intent and assigning initial
pending progress. Failure rolls back columns, receipts and schema version.
One connection per handle bounds local contention; independent handles still
rely on database constraints, not a process mutex.
Under a write lock, the driver may spend the busy timeout before returning the
context cancellation cause; it does not promise immediate interruption of that
wait. Callers must bound shutdown/retry policy accordingly.

## Acceptance and replay

`Accept` takes an already authorized scope, an opaque idempotency key, an
operation kind and canonical intent bytes. The domain caller must validate the
request, check scope existence/access and encode it deterministically. The
payload may contain secret references, never secret values. Acceptance does not
create domain objects, authorize execution or choose a runtime generation.

A unique constraint on scope and the SHA-256 key hash binds the first acceptance
to one stable operation ID, kind and immutable intent. Matching retries return
that record even after reopening the database. Changed kind or intent returns
`state.ErrConflict`, preserving the first record. Another scope has an
independent key namespace. Raw keys are not stored. `Get` also requires the
authorized scope; missing or other-scope IDs return `state.ErrNotFound`.

Insertion and conflict detection use SQLite's atomic unique constraint. No
read-before-insert race or mutable receipt is involved. A canceled or lost
response may follow a committed insert; retry the same request to recover its
ID. Acceptance retries also return the latest progress and terminal result;
they never reset a completed operation. No acceptance records are deleted or
expired by this adapter.

## Progress and terminal results

New acceptances start `pending` at version 1 with no result. The owner's `Update`
request carries the authorized scope, operation ID, expected version, its own
opaque idempotency key, target status and canonical result. Update keys have a
namespace per operation, separate from the scope's acceptance keys.

Pending operations can become `running`, `failed` or `canceled`; running ones can
become `succeeded`, `failed` or `canceled`. Success requires prior running state.
A terminal result must be nonempty; running has no result. Terminal state/result
never changes or resumes. A `canceled` record acknowledges the owner's completed
cancellation decision; writing it does not stop a worker or runtime resource.

One immediate transaction compares the version, advances the state and stores
the update key/intent receipt. Concurrent different keys at the same version
cannot both advance. Rejected updates reserve no key. A missing/foreign-scope
operation returns `ErrNotFound`; stale versions, forbidden transitions and key
reuse for another version/status/result return `ErrConflict`.

Matching retries return the **current** snapshot and report no new change, even
after later transitions. Replaying an earlier start therefore acknowledges its
receipt without restoring running state. Reusing even a historical key with a
different intent conflicts. A new key cannot rewrite a terminal result. Receipts
are retained; no pruning/expiry policy is implemented.

Storage failure rolls back both progress and receipt. Cancellation on a failed
update retains the context cause, including a busy error from transaction start
after the timeout expires. A lost response can still follow a committed update;
retry the same request to recover its receipt. The returned change flag reports
a newly committed transition, not worker ownership or execution permission.

## Limits and next increments

Recorded progress/results carry no worker lease, per-slot serialization,
generation history, revision admission or execution permission. Two different
keys can still describe
competing work; domain transactions must arbitrate that before execution.
The runtime adapters must not be exposed as durable public operations yet.

Next add generation/slot transitions and join the
[supervisor restart decision](instance-supervision.md) to durable
authorization and reconciliation. PostgreSQL/HA, typed query generation, server
configuration, agent channel and API/CLI operations follow separately. The few
SQL statements here are private to the SQLite adapter; portable state records
contain no SQLite or Kubernetes types.

Tests use real temporary disk databases, including reopen, independent handles,
concurrent acceptance/progress, historical update replay, immutable terminal
results, scoped reads, canceled locked writes, storage-failure rollback,
version-1 migration and migration rollback, literal paths and unsupported schema
versions. No live workload or production database is accessed.
