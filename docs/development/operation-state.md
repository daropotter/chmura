# Durable operation acceptance

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
rejected. One connection per
handle bounds local contention; independent handles still rely on database
constraints, not a process mutex.
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
ID. No acceptance records are deleted or expired by this adapter.

## Limits and next increments

This first record means accepted, not running or complete. It carries no worker
lease, per-slot serialization, generation history, terminal result, revision
admission or execution permission. Two different keys can still describe
competing work; domain transactions must arbitrate that before execution.
The runtime adapters must not be exposed as durable public operations yet.

Next add guarded operation progress/results and generation/slot transitions,
then join the [supervisor restart decision](instance-supervision.md) to durable
authorization and reconciliation. PostgreSQL/HA, typed query generation, server
configuration, agent channel and API/CLI operations follow separately. The few
SQL statements here are private to the SQLite adapter; portable state records
contain no SQLite or Kubernetes types.

Tests use real temporary disk databases, including reopen, independent handles,
concurrent acceptance, conflicts, scoped reads, cancellation under a write lock,
literal paths and unsupported schema versions. No live workload or production
database is accessed.
