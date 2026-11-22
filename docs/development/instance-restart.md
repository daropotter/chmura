# Instance restart execution

`k3s.Restarter.Ensure` performs one reconciliation step for an explicitly
authorized restart. `RestartRequest` supplies the original creation request, its
complete runtime identity, and the successor's explicit creation request. The
successor requires a different key/name in the same namespace. Its portable
declaration and canonical resolved artifact must remain the same; a newly
admitted node/allocation may be supplied explicitly. A restart does not silently
become a rollout or choose placement/resources.

For an automatic restart, `Supervisor.Monitor` returns `RestartNeededError`
after a confirmed process exit or the independent healthy threshold. Its
`Request(previous, next)` method binds the supervised generation and effective
plan to the explicit creation requests, without I/O. The original cause remains
available through error unwrapping. This is a decision handoff, not execution:
the owner must still complete the authorization, reporting, serialization and
durable-operation steps below before calling `Restarter.Ensure`.

## Recovering a restart request

`PrepareProcessRestart` recovers a `RestartRequest` from an owner-verified
retained `state.Process`, its exact private instance, original creation request
and explicit successor request. It performs no I/O and needs no live
`RestartNeededError`, so it works after database reopen and owner recovery.

Only canonical private v1 k3s runtime-exit or unhealthy evidence is supported.
Exit retains code zero as well as failures, and verifies the bound start and
runtime finish against the terminal occurrence. Healthy evidence requires
retained first readiness and the independent check/threshold fingerprint from
the supplied original plan. The adapter rebuilds the terminal payload with the
existing encoder and compares its exact bytes and occurrence. Missing terminal
history, startup timeout, unknown versions/engines, mixed causes, extra or
duplicate fields and malformed/noncanonical evidence produce no request.
Startup failure remains a failure, with no implicit retry policy.

Preparation validates the original runtime reference and existing restart
constraints: an explicit different successor key in the same namespace, the
same portable declaration/resolved artifact, and explicit admitted resources.
Both returned requests have independent copies of health/ports; caller mutation
cannot change the prepared snapshot. Equivalent time zones and omitted effective
defaults preserve the candidate. No retained data is migrated or rewritten.

This is a candidate, not execution authorization. The owner must verify that
the supplied process/creation intent belongs to the authorized desired state,
durably admit the restart operation, serialize it, drain all reporters and
recheck its currency before each executor call. The evidence is not a full
creation-intent record or a worker lease. The adapter does not read the desired
head, fence another owner or deduplicate obsolete restart operations.

The portable store now supplies [restart admission and current-operation
checks](slot-generations.md#automatic-restart-admission-and-currency).
The owner accepts/starts the canonical restart operation and calls `AdmitRestart`
to append its successor only from an eligible current predecessor. Before each
executor step, `GetCurrentRestart` must confirm the selected generation and
running operation. Historical admission acknowledgement alone is insufficient.
Completed/canceled, superseded or already-bound successor work conflicts before
the owner invokes runtime I/O. A bound successor requires process/result
reconciliation rather than rerunning creation. These checks do not fence races
after the read; external serialization and scope/intent authorization remain.

Tests persist/reopen terminal history, prepare without runtime calls and then
explicitly authorize the existing executor in a local fixture: retirement must
precede creation, and an exact retry after a lost creation reply recovers one
successor with fresh readiness. No live cluster is accessed.

## Driving an admitted operation

`PrepareRestartOperationIntent` freezes the validated predecessor/explicit
successor creation requests, effective plan and selected slot lineage into
canonical private v1 YAML. The owner supplies the different successor generation
ID and positive predecessor version, authorizes the scope/intent, accepts that
intent as a `restart` operation, marks it running and calls `AdmitRestart` with
the same bytes. Preparation itself admits nothing and performs no runtime I/O.
The format is engine recovery data, not a manifest escape hatch or public API.
Creation keys are retained for exact replay; secrets must never be supplied.

`Restarter.EnsureAdmitted` accepts a `RestartJournal` and explicit
`RestartSelection`. Each call validates the selected operation, historical
generation and private intent. For running work it then calls
`GetCurrentRestart`, verifies the selected predecessor/terminal evidence and
recovers the original runtime identity before invoking one `Ensure` step.
Completed/canceled, superseded or already-bound running work never reaches
runtime. Unknown, malformed or noncanonical encodings also fail before runtime.
There is no schema migration or additional local checkpoint.

While retirement is pending, the method returns `Retiring` and leaves the
operation running. Once creation is acknowledged, it records a canonical private
v1 JSON result containing the successor Pod identity and accepted-intent hash.
The transient `Created` flag is excluded so retries produce identical bytes.
The driver reserves the operation update key `restart-complete-v1` for this
transition. It exposes the identity only after validating the durable success
acknowledgement; storage failure or cancellation returns zero progress. An error
can still follow runtime creation or a committed database write, so retry the
exact selection rather than allocate a new creation key.

A succeeded operation replays its validated stored result without a runtime
client or a current-head check, including after database reopen, successor
binding/deletion or later supersession. It acknowledges historical creation,
not current existence or readiness. Replay never recreates the Pod. A running
operation retries the same idempotent creation when completion was not committed;
an already-bound successor instead requires explicit process/result reconciliation.

The owner must serialize each complete call with cancellation/supersession and
other runtime actions, stop/join old monitors and drain reporters before driving,
and wait for durable completion before binding/monitoring the successor. Current
snapshots do not fence a competing worker. Runtime and SQLite are not one atomic
transaction. Distributed ownership, bound-result reconciliation for unfinished
operations, scheduling and server/agent/API/CLI wiring remain separate increments.

## Binding the completed successor

`Resolver.BindRestartSuccessor` links a succeeded restart to its exact process
and returns a `ResumeRequest` for the existing supervision cycle. It validates
the private operation intent/result and requires the selected successor to be
the current slot head. This differs from historical creation-result replay:
an obsolete completed operation cannot authorize supervision of its old process.
The owner authorizes scope/intent, serializes the whole call with admissions and
other runtime actions, and owns discovery deadlines/backoff.

For an unbound successor, the adapter reads one Pod and verifies the completed
UID, original creation key/intent, ownership and full supported execution shape.
It discovers the application's container ID and actual process start, requiring
a nonterminating versioned snapshot with either a running process or complete
runtime-exit facts. A confirmed exit, including code zero, can be bound even
before first observation, allowing `ResumeDurable` to retain that terminal outcome.
Waiting/image pull, missing or ambiguous facts, foreign/replaced resources and
unknown termination provide no binding. Discovery does not need a probe IP;
supervision separately establishes its current network target and continuity.

The initial process observation always supplies `EverReady: false`, regardless
of runtime readiness or predecessor history. Its reserved key is
`restart-successor-bind-v1`; the portable store hashes the key and retains exact
identity/start. Only a matching binding acknowledgement and final current-head
check expose the resume request. Errors/cancellation expose zero request, even
when a database write committed. Repeat the same selection after a lost reply.

A matching live binding replays from history without a runtime client, including
after database reopen. It preserves later first readiness and the immutable
container identity/start; it never rediscovers or adopts a replacement container.
The returned plan is recovered independently from accepted intent. A terminal,
mismatched or superseded binding cannot resume. The request acknowledges retained
facts, not current existence or readiness; `ResumeDurable` rechecks the head and
observes that exact runtime generation before starting probes.

This method performs no runtime writes or creation and starts no monitor itself.
Current-head reads are not a distributed lease or fence. The owner still controls
monitor startup/shutdown, reporter draining, operation admission and restart
scheduling. A running restart with an already-bound successor continues to require
explicit result reconciliation; this handoff only accepts succeeded operations.

## Executing an authorized request

`Restarter.Ensure` validates both requests before any API calls and inspects an
existing successor for key/intent/ownership conflicts before retiring the old
generation. It then delegates to the retirement adapter. While old resource
absence is unconfirmed, the result has `Retiring: true` and no successor
identity. The caller repeats the same request with its own deadline/backoff.

If concurrent retirement returns an API conflict, the restart executor reads
once to verify the same UID/container, ownership, intent and execution shape.
An already terminating generation returns Retiring; confirmed absence permits
creation. An active generation retains the original conflict. Replacement,
foreign ownership and read errors retain their distinct causes. This recovery
never retries DELETE against a fresh resource version.

Only confirmed absence permits the idempotent creator to submit the new Pod.
Repeated or concurrent calls with the same request recover the same successor
identity, including after a lost creation response. The result is acknowledged
resource creation/reuse, not image-pull success or readiness. The new generation
starts with its own unsatisfied readiness gate; its supervisor must start with
fresh generation history and runtime facts.

Preflight checks the local payload and an existing successor, not server-side
admission or future API availability. Creation can still fail after retirement;
its original error is returned for the owner to reconcile. The operation is not
an atomic transaction across two resources. API absence is not physical fencing.

The owner must stop/join the old monitors and drain/stop the reporter before
invoking this adapter. It must serialize restart operations for the slot,
authorize them from retained runtime/health facts or an explicit user action,
and durably retire obsolete requests. An observation gap or readiness loss alone
does not authorize an automatic restart. In particular, generic runtime
unavailability is not proof of a crash and startup failure remains a failure
rather than an implicitly chosen retry policy.

This is desired-resource reconciliation, not a permanent operation receipt:
replaying an obsolete request could recreate a successor that was later deleted.
`EnsureAdmitted` adds a durable operation receipt around this primitive. Portable
operation progress/admission/currency supply the owner facts; the ownership and
integration boundaries described above remain separate work.
Tests use local HTTP API servers and real typed clients, not a live cluster.
