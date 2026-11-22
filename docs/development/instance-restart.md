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
Portable operation progress/admission/currency now supply durable owner facts.
Their engine-specific integration, distributed ownership/fencing, restart scheduling,
container-ID discovery, agent wiring and API/CLI commands remain separate work.
Tests use local HTTP API servers and real typed clients, not a live cluster.
