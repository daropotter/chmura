# Atomic deploy acceptance

`Store.AcceptDeploy` takes an authorized space/project, the expected base
version and revision ID, canonical manifest bytes and an idempotency key. The
caller validates the manifest and project identity before submitting it. The
manifest carries secret references. For a project's first deploy the base is
version zero with an empty revision ID. Later deploys use the pair returned by
`CurrentRevision`.

One SQLite transaction checks project membership and the base pair, creates a
`pending` deploy operation and an immutable snapshot, then records the desired
revision selection at the next version. The operation's private intent names
the created revision. A conflicting base or failed write leaves the previous
selection intact and creates no operation or snapshot. Each fresh accepted
deploy creates a distinct revision, including when manifest bytes are identical.

The project-scoped operation key identifies the original acceptance. A retry
with the same base and bytes returns that operation's current progress and its
original selection, even after the project advances or the database reopens.
Changing the base or bytes under an accepted key returns `ErrConflict`, as does
reusing the key of a standalone operation. Linked snapshot and selection
receipts derive from the operation ID and use a separate prefix. This lets
deploy acceptance share the existing tables with standalone snapshot creation
and revision admission. The client key is stored only as a hash.

Missing or foreign projects return `ErrNotFound`. A lost or canceled reply can
follow commit, so clients retry the exact request. Starting the operation and
tracking rollout progress belong to the execution workflow.

## Server submission

`server.SubmitDeploy` receives manifest YAML and an already authorized target.
It parses the manifest with the shared validator, loads the project by space
and project ID, and checks that the declared name matches the registered name.
A name mismatch is a project identity conflict. The current milestone supports
one application from a prebuilt image. The shared instance planner validates
the image reference and resolves effective health settings before acceptance.

The revision stores the normalized declaration using deterministic JSON-style
YAML. This preserves quoted strings, YAML field names, and duration types.
Changes that normalize to the same declaration, such as reordered mapping keys
or equivalent durations, replay under the same key. The operation can read the
saved revision to build its execution plan. The submission path performs
portable planning. Image resolution, placement and engine capabilities are
checked when preparing execution.

### HTTPS submission

`chmura-server serve --state` registers
`POST /v1/spaces/{space}/projects/{project}/deployments`. Use the stable IDs of
an existing space and project in the path. The bootstrap administrator has
access to all projects in the installation. The request requires these headers.

- `Authorization: Bearer <administrator-token>`
- `Idempotency-Key: <client-generated-key>`
- `Content-Type: application/json`

Send one JSON object of up to 1 MiB containing the integer `base-version`,
the `base-revision-id` string and the manifest YAML as the `manifest` string.
For the first deploy, send `base-version` of `0` and omit `base-revision-id`
or send an empty string. For subsequent deploys, supply the current version
and revision ID. Reading that pair through the public API is a separate
increment.

The server returns `202 Accepted` and a JSON object with `operation-id` and
`status`. Retry with the same target, key, base and manifest to retrieve the
same operation. Acceptance records desired state. Execution and rollout are
separate steps.

The server returns `401` with a Bearer challenge if the administrator
credential is missing or invalid, `400` for invalid input, `404` for an unknown
target, and `409` for a stale base or a project or idempotency conflict.
It returns `413` for a body over the size limit, `415` for an unsupported content
type, and `500` for an unexpected backend error.
