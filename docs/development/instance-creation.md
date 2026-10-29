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

This increment prepares the payload only. API writes, conflict/replay handling,
readiness-gate updates, restart actions and agent/control-plane wiring follow
separately. Image resolution and the admitted allocation are prerequisites,
rather than new defaults in the project manifest.
