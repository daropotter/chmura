# Instance retirement

`k3s.Retirer.Ensure` gracefully retires one explicit Pod/container generation.
It requires the original `InstanceRequest`, including its creation key and
intent, plus the complete runtime identity. The deterministic name must match
that request before any I/O. The stored UID, container ID, full key/intent
hashes, ownership markers and execution shape must still match before deletion.
A terminated process can be cleaned up when its container identity is retained.

The delete request uses both UID and resource-version preconditions and keeps
the configured grace period. Concurrent status changes or replacement cause a
conflict, with no automatic retry against a fresh version. No patch, adoption,
force deletion, replacement creation or local correctness cache is involved.

`RetirementResult.Absent` distinguishes confirmed absence from acknowledged or
already pending deletion. A successful DELETE does not set Absent: retry the
same generation request to verify completion. A lost response or cancellation
may leave an accepted deletion, recovered by that retry. Errors retain API and
context causes. A different UID at the same name is an error, never an object to
delete or adopt.

The owner authorizes retirement, stops and joins monitoring, and drains/stops
reporting before calling this adapter. Readiness loss or an observation gap is
not an automatic authorization to restart. Automatic restart policy and explicit
user operations belong above this primitive. A successor must wait for confirmed
absence, not merely the delete acknowledgement.

Idempotence is scoped to retirement of the selected generation under its original
creation intent. Permanent operation receipts and retiring obsolete requests
remain control-plane responsibilities. API absence is an engine fact; physical
fencing and distributed ownership require additional control-plane guarantees.
Tests use local HTTP API servers and typed clients, not a live cluster.

The [restart executor](instance-restart.md) joins confirmed retirement to explicit
creation of a new generation under a different key.
