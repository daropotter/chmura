# Readiness reporting

`k3s.NewReadinessReporter` constructs a publication worker for one complete
container generation. It requires an explicit API client, a unique owner-session
key and positive attempt-timeout/retry-interval options. Construction performs no
API calls. A new reporter must use a new session key, including when the owner
resumes observation of the same generation.

Pass `reporter.Observe` to `Supervisor.Monitor` and run `reporter.Run` separately.
Observe records only current readiness in one slot, coalesces intermediate
changes, and cancels a superseded API attempt. It never waits for outbound I/O.
`EverReady` and terminal startup history remain the supervisor owner's separate
responsibility; this worker does not persist or infer them.

Run has one lifetime and serializes requests until cancellation or a permanent
error. Each attempt is bounded by its configured timeout. It resolves the exact
generation and uses an explicit resource version and idempotency key with the
readiness writer. Ambiguous transport/timeout failures retain the original
request for replay, recovering an accepted write without issuing a different
intent under the same key. Resource-version conflicts require a new snapshot
and key for the current local result. Failed attempts wait the configured retry
interval; a newer readiness result interrupts that delay. No local update can
be lost when an older attempt completes.

Replacement, unavailability, foreign ownership and permanent API errors such as
forbidden/not-found stop reporting with their original causes. The worker does
not adopt, create or restart resources. Cancellation can race server acceptance;
a newer observation is reconciled by a following write. This is one owner's
publication worker, not a distributed supervisor lease or permanent operation
history. It does not periodically poll after an acknowledged result.

`Flush(ctx)` waits for acceptance of the state observed at entry, or a coalesced
newer state. All concurrent waiters are notified; worker failure/cancellation
unblocks them with its cause. An already accepted state remains acknowledged.
Acceptance means the custom Pod condition was verified, not that routing or a
public status API is implemented.

Keep the reporting context alive while monitoring stops. After Monitor returns
and has published its final local unready state, call Flush with a bounded
context, handle any publication failure, then cancel and join Run. Canceling the
reporter first does not guarantee delivery of that final state. Probe callbacks
must never call Flush or otherwise wait for publication.

Tests use local HTTP API/watch and application servers with real typed clients.
They cover coalescing, blocked/canceled I/O, retry/backoff, lost responses,
version conflicts, permanent errors, concurrent flushes and draining the final
state from a supervisor. Agent wiring, durable history/ownership, automatic restart
triggering and API/CLI deployment remain separate increments.
