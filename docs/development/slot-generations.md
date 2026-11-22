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

## Automatic restart admission and currency

`sqlite.Store.AdmitRestart` accepts a `state.RestartAdmissionRequest`: the
generation request plus an explicit predecessor ID and positive expected slot
version. The owner first accepts a canonical private intent with kind
`state.RestartOperationKind` (`restart`) and explicitly marks that operation
running. The successor's intent must exactly equal the accepted operation's
intent. Storage creates neither the operation nor observed successor facts.

The same immediate transaction used for generation admission verifies that the
selected predecessor is the current expected head, has a bound terminal process
with `runtime-exit` or `unhealthy` cause, and the restart operation is running in
the same scope. Unhealthy history requires retained first readiness. Live,
unbound or startup-failed predecessors cannot automatically restart. The owner
validates engine-specific terminal evidence, desired intent and authorization;
the store never interprets private engine payloads or chooses a retry policy.

Existing constraints serialize competing successors. Exact receipt retries also
verify the immutable predecessor and operation/intent association, then return
the current head without another admission, even after operation completion or
supersession. They never restore the old head. Rejections reserve no generation
or admission key; a lost/canceled response can follow commit, so retry exactly.
This uses the existing version-5 tables, with no migration or new receipt format.

Before each authorized creation/reconciliation step, the owner calls
`GetCurrentRestart(scope, slot, generation, operation)`. One transaction requires
that exact generation/operation at the current head, a running restart operation
with matching intent, and an eligible closed predecessor. It returns the
generation, operation and predecessor/process snapshots together. Foreign or
missing data provides no assumed state; completed/canceled or superseded work
conflicts and cannot be revived by an admission receipt.

An already bound successor also conflicts, whether live or terminal. Once an
exact process identity is known, rerunning creation could recreate a deleted
resource under an immutable generation binding. The owner must recover/finalize
from existing process history instead. Receipt acknowledgement still works.

This check can become stale after its transaction. The owner must serialize all
runtime actions, stop/drain reporters and prevent cancellation/supersession from
racing execution. No snapshot is a lease or distributed fence. Payload codecs,
execution loops and terminal-result recovery remain owner responsibilities;
this API does not execute a runtime call or automatically finish an operation.

## Boundaries and next steps

This is desired state, not observed process identity, readiness, worker ownership
or proof that the previous runtime stopped. Admission performs no cluster I/O,
chooses no placement, and neither retires nor creates a runtime resource. Slot
versioning serializes desired admissions; it does not fence concurrent workers
or authorize the runtime executor. A returned snapshot can become stale after
the transaction, so callers must not treat it as a lease.

[Retained process history](process-history.md) now binds immutable process facts
and sticky first readiness and terminal causes separately from desired intent.
Restart admission and the current-operation check now join those facts to a
durable successor selection. The owner still needs execution ownership/fencing,
engine-intent encoding and reconciliation/finalization rules.
Server/agent/API/CLI integration remains separate work.

Tests use temporary disk databases and independent handles. They cover competing
slot updates, identical retries, stale/rebound requests, immutable history and
reopen, scoped reads, operation gating, cancellation under a write lock, raw-key
non-persistence and migration from the frozen version-2 schema. No live cluster
or production database is accessed.
