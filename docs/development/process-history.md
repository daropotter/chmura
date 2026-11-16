# Retained process identity and first readiness

`state.Process` retains a desired generation's exact runtime identity, actual
start time and `EverReady` fact. `sqlite.Store.RecordProcess` records
owner-verified facts; it performs no runtime discovery or probe execution.
Identity is a canonical private engine reference containing complete immutable
process identifiers, never a manifest escape hatch, credentials or native
readiness flags. The owner authorizes scope and validates this reference.

The first observation binds the identity and nonzero start time permanently.
Neither can change, and one identity cannot bind another generation within the
same scope. Times use UTC RFC3339 with nanosecond precision; equivalent time-zone
presentations do not create a conflict. A fresh observation requires an existing
current desired generation. It may arrive after the deployment operation has
completed, because supervision continues independently of that operation.

## First readiness is sticky history

`EverReady` may only come from Chmura's full first-readiness threshold for this
exact process. It can change from false to true and never back. A later false
observation cannot erase it. A successor has its own binding and no inherited
first readiness; attempting to reuse the predecessor's identity conflicts.

This fact is not current traffic readiness. On resume, the existing
`MonitorReadinessWithHistory` starts unready, rechecks the full threshold and
uses the retained fact only to avoid rearming startup timeout and to preserve
healthy activation. Only committed facts survive a database reopen. The owner
must reconcile missing history explicitly; a missing binding is `ErrNotFound`,
not evidence of current readiness or startup failure.

## Receipts and concurrency

An immediate transaction binds/merges facts and records the observation's
hashed key. Keys are per generation. The recorded boolean reports a newly
committed receipt, even if the same facts were already known. Matching retries
return the retained snapshot without a new receipt; changing readiness under
the same key, or any identity/start-time mismatch, returns `ErrConflict`.
Concurrent true/false facts for the same process retain true; competing process
identities cannot both bind.

Exact historical retries may acknowledge a receipt after the generation was
superseded, but new observations for it conflict. Historical reads/replays grant
no right to resume that process. A canceled/lost response may follow commit;
retry the exact request. Storage failure rolls back both binding/readiness and
receipt. The existing SQLite busy-timeout cancellation latency still applies.

Schema version 4 adds process bindings and receipts without fabricating runtime
facts for existing desired generations. Migration from version 3 preserves
generation history and operation progress/results; failure rolls back the new
tables and version together. No fact or receipt expiry is implemented.

## Integration boundary

Run persistence in an owner/reporting worker outside probe callbacks; those
callbacks must remain prompt and perform no blocking I/O. Queueing, draining,
failure handling and acknowledgement policy still belong to owner integration.
This library does not persist terminal process causes, acquire execution
ownership, fence old workers, publish routing readiness or execute a restart.
A generation already declared failed must not be resumed.

Next retain terminal causes and connect this history to the supervisor owner's
authorization/reconciliation path. Agent/channel and API/CLI integration remain
separate work. Tests use real temporary databases, virtual-time readiness,
independent handles and fault injection; no live cluster or production database
is accessed.
