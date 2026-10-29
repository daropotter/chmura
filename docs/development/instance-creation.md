# Instance creation

`internal/engine/k3s.PrepareInstance` translates a portable instance plan into
one Pod. `InstanceRequest` supplies the target namespace, an idempotency key,
the resolved image and an already admitted node/CPU/memory allocation. Namespace
authorization, isolation/admission policies, node selection and capacity checks
belong to the owner. This adapter does not choose or create them implicitly.

Execution requires a named image digest from the declared repository. A pinned
source cannot change digest; a tagged source must be resolved by the owner.
The execution reference is canonicalized to repository plus digest, while the
portable declaration stays unchanged. Preparing a payload does not contact a
registry or verify its contents.

The Pod name is derived from the namespace-scoped request key. Metadata contains
full key and versioned intent hashes, including logical names, health rules,
ports, image and allocation, for the writer to detect key reuse or collisions.
The raw key is not stored. Repeating a request produces the same payload without
local state or generated timestamps.

The selected allocation becomes equal resource requests and limits. NodeName
binds directly to the admitted node; this is not a capacity or scheduling check.
Logical port
names remain in the plan; native container ports use numeric TCP/UDP listeners,
deduplicating aliases without publishing host ports or services. The Pod receives
no implicit service environment or service-account token mount.

One Pod represents one generation. `restartPolicy: Never` leaves crash/health
restart decisions to Chmura's supervisor. A `chmura/ready` readiness gate starts
unsatisfied and must be updated by that supervisor; the adapter never substitutes
native startup/readiness/liveness probes for Chmura rules. The initial executor
supports explicit HTTP/TCP readiness and optional HTTP/TCP or process-only
healthy monitoring. Unsupported checks fail before resource creation.

`Creator.Ensure` prepares the payload and submits a Pod create request through
an explicitly configured client. On AlreadyExists it reads the stored Pod and
checks full key/intent hashes, ownership markers and execution shape. Equivalent
resource quantity representations and admitted namespace/defaulting context
(default service account, pull secrets, tolerations and empty/default security,
priority and user-namespace fields) do not cause a false conflict. The container
spec and other execution fields must still match. A conflict returns
`ErrInstanceConflict` without patches, deletions or adoption.

The result contains the Pod UID/name and application container name. It means
the resource was accepted or its existing identity verified, rather than that
an image was pulled or readiness acquired. A supervisor must discover the
container ID before resolving/observing the running generation. A repeated
request reuses an existing terminated Pod too; a new generation needs a new key.

Cancellation or a lost response can leave an accepted Pod. Repeating the same
request/key finds that identity without an additional current Pod, including
under concurrent writers. API errors retain their cause; missing namespaces are
not created. No local cache or mutex is needed for correctness.

This is idempotence of the current desired resource, not a permanent operation
receipt. If a Pod has been deleted, Ensure can recreate it. Before exposing this
through the public API, durable control-plane operations must retain results and
retire old requests so replay cannot resurrect obsolete work. Image resolution,
admission, readiness-gate updates, restart actions and agent/control-plane wiring
remain separate responsibilities. No live cluster was changed by the tests.
