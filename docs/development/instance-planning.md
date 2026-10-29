# Instance planning

`internal/deployment.PlanInstance` builds a portable configuration snapshot for
one explicitly selected application from a project validated by
`manifest.Parse`. It is shared planning logic for future CLI/control-plane
callers, before placement, runtime identity and resource creation.

The plan contains the declared project/application names, prebuilt image,
declared ports, effective health configuration and an explicit readiness mode.
Names are preserved; selecting a missing application is an error, even if the
project has only one application. A Dockerfile source returns `ErrBuildRequired`
instead of attempting a build or guessing an image. A caller deploying a whole
project must plan every selected application successfully before executing any
of them.

Image-reference syntax is checked with `distribution/reference`. The declared
reference is retained exactly: planning does not insert a registry or tag, look
up a registry, pull an image or resolve a tag to a digest. A tagged reference
remains mutable; immutable revision resolution is a later responsibility.

Health settings use the shared `Health.Effective` defaults. When `health` is
omitted, the plan resolves an empty lifecycle configuration so its startup and
readiness defaults are explicit. The declaration remains untouched, and absent
probes and healthy rules stay absent. Readiness mode is `checked` for an explicit
readiness probe and `process-only` otherwise. An independent healthy check does
not become a readiness check. These modes describe the intended checks, rather
than a runtime readiness result or proof of engine support.

Ports and all nested health payloads are independent copies. Separate plans and
the source declaration can be used without sharing mutable configuration.
Repeating planning with unchanged inputs produces the same values, without
timestamps, generated identifiers or local-state dependencies.

This initial plan covers the currently parsed application subset. It does not
confirm remote space/project existence or allocate resources. Placement,
capability checks, image resolution, runtime creation/idempotency, process-only
readiness execution and agent integration follow in later increments. There is
no `deploy` command, deployment endpoint or cluster mutation in this increment.

The [k3s instance adapter](instance-creation.md) accepts this plan together with
explicit image-resolution, target and allocation facts to prepare its Pod.
