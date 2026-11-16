# Retained process history

`state.Process` retains a desired generation's exact runtime identity, actual
start time, `EverReady` fact and optional terminal decision.
`sqlite.Store.RecordProcess` records owner-verified facts; it performs no runtime
discovery or probe execution.
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

## Terminal supervision history

`RecordProcessTerminal` closes an already bound current generation with one
immutable `ProcessTerminal`: a cause, nonzero occurrence time at/after process
start and nonempty canonical private evidence. It verifies the exact bound
identity/start. The owner authorizes scope and validates the evidence; the store
does not interpret it or query the runtime. Evidence must contain no credentials.

The closed set of causes preserves different meanings:

- `runtime-exit`: confirmed exit of this exact process, including exit code zero.
  The owner retains the runtime exit code, reason, finish time and observation
  evidence in the canonical payload. Exit may precede first readiness.
- `startup-timeout`: Chmura's startup deadline failed before first readiness.
  It conflicts if `EverReady` is already true.
- `unhealthy`: Chmura's independent healthy threshold failed after first
  readiness. It conflicts unless `EverReady` is already true; ordinary loss of
  readiness is not this cause.

Observation loss, unavailability, replacement and owner cancellation are rejected
as terminal causes. They remain reconciliation/ownership concerns and provide no
crash or restart evidence. A healthy or startup failure does not prove that the
runtime has physically stopped. No cause alone authorizes a restart.

The first terminal decision and its hashed key are one atomic record/receipt.
Only that exact key, cause, time and evidence may replay; another key or changed
facts conflict. Terminal keys are scoped per generation separately from readiness
observation keys. Exact terminal retries remain valid after supersession and
deployment-operation completion. Fresh terminal writes for obsolete generations
conflict. A missing binding returns `ErrNotFound`, fabricating no failure.

`GetProcess` reads binding, readiness history and terminal fact in one snapshot.
Old accepted readiness receipts also return that terminal snapshot. After a
terminal commit, every fresh `RecordProcess` conflicts, even if its facts would
otherwise be unchanged. Terminal decisions cannot be cleared or replaced, and
successors inherit neither terminal nor readiness history. Concurrent first
readiness and startup failure serialize: only one can win. Failed writes reserve
no terminal/key; a lost/canceled response can follow commit, so retry exactly.

The owner must stop and drain its reporters and persist first readiness before
recording terminal failure. On recovery it must check `Terminal` before invoking
the supervisor: a closed generation must not be monitored again, even if runtime
discovery still finds it running. These records are not worker leases or fencing;
owner authorization and serialization remain required for runtime actions.

Schema version 4 adds process bindings and receipts without fabricating runtime
facts for existing desired generations. Migration from version 3 preserves
generation history and operation progress/results; failure rolls back the new
tables and version together. No fact or receipt expiry is implemented.

Schema version 5 adds terminal history without synthesizing failures for older
bindings. Migration retains existing process identities, readiness receipts and
operation results; the entire migration chain rolls back on failure.

## Integration boundary

Run persistence in an owner/reporting worker outside probe callbacks; those
callbacks must remain prompt and perform no blocking I/O. Queueing, draining,
failure handling and acknowledgement policy still belong to owner integration.
This library acquires no execution ownership, fences no old workers, publishes
no routing readiness and executes no restart. A terminal snapshot is retained
history and can become stale with respect to the current desired head.

Next connect this history to the supervisor owner's authorization/reconciliation
path, including concrete private engine identity/evidence encoding and durable
reporter acknowledgement. Agent/channel and API/CLI integration remain
separate work. Tests use real temporary databases, virtual-time readiness,
independent handles and fault injection; no live cluster or production database
is accessed.
