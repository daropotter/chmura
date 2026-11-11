# Durable desired generations

`state.Generation` records immutable desired intent for one stable application
slot. `sqlite.Store.AdmitGeneration` appends it before runtime reconciliation.
Scope, slot and generation IDs are explicit owner-supplied identities; the
store does not normalize names or create spaces/projects.

A slot is represented by its generation history. `ExpectedVersion: 0` explicitly
creates its first desired generation; later admissions must match the current
slot version. The highest admitted version is the current desired head.
`CurrentGeneration` reads that head; `GetGeneration` reads a historical record
by scope, slot and ID. Missing/foreign records return `state.ErrNotFound`.

## Admission and replay

An admission supplies a scope, slot ID, new generation ID, operation ID,
idempotency key, expected slot version and canonical desired intent. The owner
must authorize the scope and verify that the operation's domain intent permits
this admission. Payloads contain secret references rather than values. The
store requires an existing same-scope operation in `running` state for a new
admission; pending or terminal operations cannot append work.

One immediate transaction validates the operation and slot version and appends
the generation. Unique constraints enforce one generation per slot/version,
one admission per operation/slot, generation IDs unique within a scope, and
permanent key bindings within a slot. Concurrent different operations expecting
the same version cannot both advance. The store never retries with a newer
version on the owner's behalf.

Matching key retries compare generation ID, operation, base version and intent,
then return the **current** head with no new admission. This remains true after
operation completion or later admissions: an old retry cannot restore an old
head. Changed intent under an old key conflicts. A new key cannot admit another
generation for an already used operation/slot or rebind an existing generation
ID. Validation/conflict rejections reserve no identity, key or version. A
canceled or lost response may follow committed admission; repeating the exact
request recovers its receipt and the current head. Raw keys are hashed and never
stored.

Schema version 3 adds this append-only history transactionally. Migration from
version 2 retains operation identities, progress, results and update receipts;
new databases use the same migration chain. No generation records expire or are
deleted by this adapter.

## Boundaries and next steps

This is desired state, not observed process identity, readiness, worker ownership
or proof that the previous runtime stopped. Admission performs no cluster I/O,
chooses no placement, and neither retires nor creates a runtime resource. Slot
versioning serializes desired admissions; it does not fence concurrent workers
or authorize the runtime executor. A returned snapshot can become stale after
the transaction, so callers must not treat it as a lease.

Next bind each desired generation to exact observed runtime facts and retained
first readiness, then join supervisor decisions to authorized restart
reconciliation. The owner still needs execution ownership/fencing and operation
retirement rules. Server/agent/API/CLI integration remains separate work.

Tests use temporary disk databases and independent handles. They cover competing
slot updates, identical retries, stale/rebound requests, immutable history and
reopen, scoped reads, operation gating, cancellation under a write lock, raw-key
non-persistence and migration from the frozen version-2 schema. No live cluster
or production database is accessed.
