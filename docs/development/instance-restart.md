# Instance restart execution

`k3s.Restarter.Ensure` performs one reconciliation step for an explicitly
authorized restart. `RestartRequest` supplies the original creation request, its
complete runtime identity, and the successor's explicit creation request. The
successor requires a different key/name in the same namespace. Its portable
declaration and canonical resolved artifact must remain the same; a newly
admitted node/allocation may be supplied explicitly. A restart does not silently
become a rollout or choose placement/resources.

The adapter validates both requests before any API calls and inspects an
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
Durable operation state, distributed ownership/fencing, restart scheduling,
container-ID discovery, agent wiring and API/CLI commands remain separate work.
Tests use local HTTP API servers and real typed clients, not a live cluster.
