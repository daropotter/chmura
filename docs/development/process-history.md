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

## Resuming k3s supervision

`k3s.Supervisor.Resume` reads the current desired head and its retained process
before contacting the runtime. The owner supplies an explicit scope, slot,
generation, exact private instance and validated plan authorized for that desired
intent. Resume does not decode/validate the plan against opaque generation
intent, discover a container identity or adopt another process. A missing binding
requires explicit owner reconciliation; it never falls back to `EverReady=false`.

`EncodeProcessIdentity` supplies the stable version-1 canonical private k3s
reference for new bindings: engine discriminator and complete namespace, pod
name/UID and container name/ID. Mutable IP, resource version and readiness are
excluded; actual start time remains a separate immutable binding fact. A frozen
identity fixture protects compatibility with existing bytes. Incomplete or
invalid UTF-8 references are rejected instead of producing ambiguous identities.
Other or future encodings require explicit reconciliation, never implicit adoption.

Resume rejects superseded generations, terminal history and identity mismatches
with `state.ErrConflict`, before runtime I/O. Missing scope/slot/process records
and storage errors retain their causes. Deployment-operation completion does not
stop ongoing supervision. The runtime snapshot must also match retained start
time before any probes run. A changed start becomes `ErrObservationLost`, including
when the first snapshot already reports an exit; it is never restart evidence.
A matching reported exit still returns the existing typed restart handoff.

When retained `EverReady` is true, startup timeout stays disabled and independent
healthy checks are activated. Current readiness starts false and must earn the
full fresh success threshold. Resume inherits Monitor's cancellation, final readiness
revocation and joined watch/probe cleanup. It records no new history or status.

These are snapshot checks, not atomic ownership acquisition or fencing. The owner
must serialize admissions/terminal decisions with monitor startup/shutdown, keep
one authorized owner and stop an obsolete monitor. Resume does not poll desired
state during monitoring or deduplicate concurrent callers. Durable reporting and
restart execution remain separate owner work.

## Acknowledging first-readiness persistence

`supervision.ProcessReporter` runs process binding and first-readiness writes in
an owner worker. `NewProcessReporter` freezes an explicit authorized observation
and positive write-attempt/retry bounds; construction performs no I/O. The owner
validates identity/start and any initial `EverReady=true` evidence. The initial
binding is always written before a subsequent first-ready receipt, even if the
callback observes readiness before `Run` starts.

`Observe` is suitable for the supervisor callback: it retains only monotonic
`EverReady` in bounded memory and performs no I/O or write wait. Repeated values
or later false observations enqueue no extra facts. Current `Ready` and
`StartupFailed` flags do not invent readiness history or terminal evidence.

`Flush` waits for acceptance of the initial binding and all facts observed at its
entry. A nil result requires successful storage acceptance or exact receipt
replay. Until then, queued facts may be lost on owner failure; they cannot
authorize a later action. A bounded flush context can expire while `Run` remains
alive. The error retains both context classification and the last storage failure
when available, so a persistent write problem remains diagnosable.

The worker runs once. Attempt deadlines and retry pauses are explicit; SQLite's
existing busy-wait cancellation latency still applies. Each fact has a separate
stable key derived from the hashed session key, and every ambiguous retry keeps
the exact payload/key. Recovery must repeat the initial observation and session
key. A lost/canceled response may follow commit. Conflict/not-found errors and
terminal snapshots stop the worker and wake pending flushes; other write errors
retry until cancellation. Historical receipt acknowledgement grants no execution
permission for an obsolete generation.

The owner must keep persistence alive independently of monitor cancellation:

1. Stop/join the supervisor and its callbacks.
2. Call `Flush` with a bounded context while the history worker is still running.
3. Cancel/join the history worker; stop/drain runtime readiness publication too.
4. Record the verified terminal cause with its own stable key, then reconcile any
   restart only under explicit owner authorization.

An unacknowledged flush requires reconciliation, not continuation as if history
were durable. The reporter neither classifies terminal causes nor writes routing
readiness; it acquires no ownership/fencing and performs no restart. The owner
still controls worker lifetimes, failure handling and serialization. Integration
tests exercise resume → first readiness → exit → flush → stop/join → terminal
record against temporary SQLite and a local typed runtime API.

## Preparing terminal evidence from a supervisor outcome

`k3s.PrepareProcessTerminal` converts an owner-verified completed outcome into a
`state.ProcessTerminalObservation` without I/O. The owner supplies the authorized
scope/plan, acknowledged process snapshot, exact instance and stable terminal
key. It joins the supervisor/reporters and flushes first readiness before using
this adapter. The prepared observation then goes to `RecordProcessTerminal`;
preparation alone acknowledges no durable write and grants no runtime permission.

The version-1 private canonical JSON uses a k3s discriminator and one cause:

- Runtime exit requires a complete matching `InstanceExitError`, exact bound
  start, resource version and valid finish time. Evidence retains exit code
  (including zero), reason and UTC start/finish facts. Occurrence time is the
  runtime finish; later receipt of the same exit does not change the event.
- Startup failure requires the owner-verified `ErrStartupTimeout`, a supported
  validated readiness plan and no acknowledged first readiness. The observation
  must be at/after the original deadline. Occurrence time is that deadline,
  calculated from actual process start and the effective startup timeout.
- Healthy failure requires the supervisor's `RestartNeededError`, acknowledged
  first readiness and its original instance, observed start and effective plan.
  The supervisor now retains that start in the private decision snapshot. Evidence
  retains the independent failure threshold and check fingerprint. A bare
  `ErrUnhealthy` sentinel supplies no generation-bound supervisor decision.

For health decisions, `ObservedAt` is the owner's time of receiving the completed
outcome. Capture it once and preserve it on retries. It is not a claim of the
exact probe-completion instant. The adapter stores only a SHA-256 fingerprint of
the effective check and selected endpoint; raw probe paths, commands and error
diagnostics are excluded. Frozen exit/fingerprint fixtures protect canonical
v1 compatibility and equivalent time zones/defaults produce identical evidence.

Cancellation, deadlines, observation loss, replacement, generic unavailability
and ambiguous outcomes return `ErrNoTerminalEvidence` with no observation. The
owner retains the original cause for cancellation/reconciliation. Changed process
identity/start or a different healthy plan conflicts. Malformed facts/plans are
rejected before any storage call. These checks rely on the owner verifying the
outcome's provenance and the plan's association with desired intent.

Use the exact prepared key/payload to recover from a lost response, including
after database reopen. Preparation can use a retained terminal snapshot for an
exact historical replay; storage remains authoritative about current-head
admission, terminal immutability and whether the key really matches its receipt.
No terminal encoding change migrates or rewrites older retained facts.

## One durable supervision cycle

`k3s.Supervisor.ResumeDurable` joins the existing resume, process reporter and
terminal adapter for one already bound current process. The caller supplies an
authorized `ResumeRequest`, stable history-session and terminal keys, positive
write-attempt/retry bounds and a finalization timeout. Invalid configuration,
unsupported plans and missing, mismatched or closed bindings perform no writes
or runtime I/O. No missing binding is created implicitly.

The cycle owns these steps:

1. Acknowledge the initial process-history receipt before starting runtime
   observation. This wait uses the write-attempt timeout. Its initial
   `EverReady=false` payload stays stable when reusing a session after recovery;
   false cannot erase committed true, which Resume reloads before monitoring.
2. Run the monitor and process-history worker together. The observer only queues
   sticky first readiness. A permanent history failure cancels and joins the
   monitor; monitor/parent cancellation leaves the history worker alive to drain.
3. Capture outcome-receipt time once after the monitor joins. Flush observed
   first readiness with a detached bounded context, then cancel/join the worker.
4. Reload acknowledged history, prepare terminal evidence and retry its exact
   key/payload until acknowledged or finalization fails. The final flush, read
   and terminal retries share the finalization budget. Store cancellation
   latency still applies, and started workers are joined before return.

A nil method error returns `DurableSupervisionResult` with the acknowledged
terminal process and original `Outcome`. Exit/healthy decisions retain their
typed restart classification; startup timeout retains its distinct meaning.
Every method error returns a zero result. Failed flushes, terminal writes,
cancellation and observation loss never expose an unacknowledged restart
decision through that result. Nonterminal stops preserve known first readiness
without inventing a terminal record. Terminal retry exhaustion retains the last
storage failure alongside the finalization deadline.

Parent cancellation prevents terminal continuation, including when it arrives
during draining or a terminal write. A canceled/lost response can still follow
a commit: read retained history and reconcile explicitly. A later call rejects
an already terminal generation; it does not resume or reissue that decision.

This method owns only process-history reporting. It publishes no routing
readiness and invokes no external observer. The caller must serialize ownership,
admissions and runtime actions, and stop/drain any other reporters before acting.
Terminal acknowledgement is neither ownership/fencing nor restart permission.

## Integration boundary

ProcessReporter supplies bounded first-readiness queueing and acknowledgement,
and PrepareProcessTerminal encodes verified outcomes for durable terminal writes.
ResumeDurable orchestrates one monitor and its process-history worker; the
surrounding owner lifecycle and failure/reconciliation policy remain owner work.
This library acquires no execution ownership, fences no old workers, publishes
no routing readiness and executes no restart. A terminal snapshot is retained
history and can become stale with respect to the current desired head.

The recovery read path, first-readiness acknowledgement and terminal adapter
now form one durable supervision cycle. Runtime-readiness publication and
authorized restart reconciliation remain subsequent owner work.
Agent/channel and API/CLI integration remain separate work. Tests use real
temporary databases, virtual-time readiness, independent handles and fault
injection; no live cluster or production database is accessed.
