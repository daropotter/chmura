# Project revision admission

`state.RevisionSelection` records the current admitted desired revision of an
existing project: snapshot ID, operation ID, monotonically increasing version
and base revision ID. It records a control-plane decision, separately from
observed rollout state. A selected revision is not proof that instances run it.

`sqlite.Store.AdmitRevision` takes an explicit space/project, existing snapshot,
running operation, idempotency key and expected base pair. The first selection
uses version `0` and an empty revision ID. Later requests carry both version and
revision ID from `CurrentRevision`, read in the same command. Both must match;
the version also catches changes that select a retained snapshot again. Invalid
base pairs and versions that would overflow are rejected before admission.

The caller authorizes project access and validates the operation's kind/intent,
target manifest and meaning of the selection. The adapter checks project/space
membership, snapshot ownership and a `running` operation scoped to the project
ID. Missing/foreign records return `state.ErrNotFound`; incompatible progress,
base mismatches or receipt conflicts return `state.ErrConflict`. Every error
returns a zero selection and `admitted=false`.

One immediate transaction checks these records and appends a selection that is
also its creation receipt. The current head is the highest admitted version;
there is no separately updated pointer that can lose its receipt. Project-scoped
uniqueness covers version, hashed admission key and operation ID. One operation
can select at most once; rejected requests reserve no otherwise unused key.
Independent handles rely on database transactions/constraints, not a process
mutex. Raw admission keys are not stored.

An exact retry matches snapshot ID, operation ID and the complete original base
pair, then returns the **current head** with `admitted=false`. It can succeed
after operation completion and later selections, including database reopen. It
does not restore the original target, resume the old operation or revalidate its
now-terminal progress. The returned head may belong to a later operation, and
may advance again after the call; it is not runtime execution permission.

`CurrentRevision(spaceID, projectID)` filters the already authorized space and
project. No admissions, missing projects and foreign scopes return
`state.ErrNotFound`; existing snapshots do not imply a selection. Admission
preserves all immutable snapshot bytes and leaves operation progress unchanged.
A failed rollout therefore retains its declaration; later selection is explicit.

Schema version 10 adds the initially empty selection history and a unique
project/snapshot index for its composite foreign key. Migration preserves prior
snapshots and operations, infers no head, and rolls back the chain on failure.
Cancellation can outlast the SQLite busy timeout; a lost/canceled response can
follow commit, so callers retry exactly rather than assuming rollback.

For `AdmitRevision`, snapshot creation and starting the existing operation
happen before admission. Use [deploy acceptance](deploy-acceptance.md) to create
a snapshot, pending operation and desired selection together. Rollout and
worker ownership are separate contracts. Tests use temporary SQLite databases
and independent handles, including stale/retained-base cases, historical retry,
operation scope/progress, contention, storage failure, locked cancellation and
migrations.
