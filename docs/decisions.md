# Design decisions

These are the settled choices that shape the model, grouped by area. Each is one
we do not expect to revisit lightly; where the reasoning is subtle it is recorded
alongside the decision so the *why* survives. They reflect the model as
documented — not a historical log — so terminology matches the rest of these docs.

## CLI and UX

1. The CLI is the project's primary UX. The API and web UI are clients over the
   same control plane, never separate sources of logic.
2. Every operation has a non-interactive form. The CLI never starts asking for
   input on its own.
3. Interaction requires `--interactive` / `-i`.
4. `gum` is an optional interactive backend, never a place for business logic.
5. `Ctrl+C` cancels the whole wizard and never triggers a fallback.
6. `--dry-run` always guarantees no state change. There is no `-n` shorthand.
7. `--quiet` suppresses normal stdout but never hides errors on stderr.
8. Help is hierarchical.
9. A confirmation is not data collection: the command prints the full list of
   effects, asks only at a terminal, and without a terminal and without `--yes`
   it errors rather than waits.
10. `stdout` is results; `stderr` is errors, diagnostics, and interaction.
    `--output` is a machine format, not a substitute for `--quiet`.
11. Exit codes separate success, execution error, argument/configuration/missing-
    confirmation error, a retryable state conflict, a `DEGRADED` result under
    `--fail-on-degraded`, and `Ctrl+C`.
12. A `DEGRADED` result is success by default; `--fail-on-degraded` changes only
    the exit code.

## Addresses and local state

13. There is one address: `[remote:]space/project/application`. There is no
    separate local and remote syntax.
14. A partial address's meaning follows the command's subject and segment count,
    filled from the right.
15. Application operations have their own `app` group, so a project command never
    takes the `project/application` form.
16. A space is always explicit or comes from a project's remembered target. It is
    never guessed.
17. No `default` space. A space is created only by `chmura space create`, with an
    explicit name and cluster.
18. Space names are unique within an installation.
19. A remote is a named connection to one installation and nothing more; it holds
    no space. There is no separate "profile" concept.
20. No command takes both an address and a path in the same position; paths need
    no `./` prefix.
21. Local state is never required for correctness; losing it costs only
    convenience.

## Project identity

22. A project's identity is a server-assigned `project-id`; its recognition key is
    `(space, name)`, with the name unique within the space.
23. `name` is required in `chmura.yaml` and never recomputed at deploy.
24. Deploy never creates a project or space; creation is explicit (`--create` or a
    create command).
25. Renaming a project is `chmura project rename`, not a manifest edit.
26. `.chmura/state.yaml` holds the remembered target and last-seen `project-id` as
    a safeguard, not a source of truth.

## Imperative vs declarative

27. State-changing commands are operations, envelope choices, or a deploy — each
    in exactly one category.
28. The manifest declares an envelope; imperative commands choose a value within
    it; only a deploy changes the envelope.
29. There is no imperative command for a value the manifest gives no range for.
30. An imperative choice is stored as `overrides` — a separate section of the
    remote object, not a change to `spec`.
31. A deploy never silently clears overrides; an override outside the new envelope
    stops the deploy.
32. An override creates no revision and no revision conflict.
33. An override may carry an expiry (`--for` / `--until`); on expiry it reverts to
    the declared envelope.
34. `chmura project export` returns `spec` without overrides.

## Values and secrets

35. Values are literals in the manifest, or `var`/`secret` outside it.
36. The manifest declares which values an application needs; the deployment holds
    what they are.
37. `var` and `secret` have two scopes, project and space. The manifest carries no
    scope — just a name.
38. A reference resolves project, then space, then error. Project shadows space.
39. A `secret:` reference never resolves to a `var`, or vice versa.
40. A project's secret is invisible to other projects; resolution goes up, never
    sideways.
41. Every value's provenance is shown in the plan, status, and deploy log.
42. `env` exists only at the application level; no inheritance or merging.
43. A secret is writable and never readable; there is no `chmura secret get`.
44. `chmura secret set` takes no value in its arguments — only a file or stdin.
45. A value's kind is carried by the flag name (`-e/--env`, `--var-env`,
    `--secret-env`), never a prefix in its value.
46. Values are never created automatically; a missing reference stops the deploy
    before any change.
47. Changing a secret creates no revision and by default restarts nothing;
    automation needs `on-change: restart`, and `--restart-affected` rolls affected
    apps on `set`.
48. The secrets engine is replaceable; the user contract does not depend on the
    driver, and the encrypting key never lives in the state database.

## Manifest fields

49. A field that varies by deployment (today, an endpoint hostname) may be a `var`
    reference, resolved at plan time, never a `secret`. There is no full
    templating — the manifest stays self-describing.
50. A range field takes a `min`/`preferred`/`max` object or a scalar (fixed); the
    same rule covers `resources`, `instances`, and volume `size`.
51. A range may omit keys, filled by fixed rules. `max` is never unbounded.
52. `chmura init` infers only from `Dockerfile` and `docker-compose`, never from
    application behavior; what it cannot derive it writes as commented scaffolding,
    and it never copies a secret value.

## Health and readiness

53. Readiness and health are two different questions. `health.check` is the
    readiness probe; `healthy` is the health rule.
54. There is one `check` mechanism and rules over its results; there is no separate
    startup probe.
55. `healthy` does not apply until `ready` succeeds once; no readiness within
    `startup-timeout` means the instance failed.
56. `healthy` is opt-in; without it, only a process crash restarts. It watches the
    process only unless given its own `check`, and never the readiness check.
57. A check is `http`, `tcp`, `process`, or `exec`, and names a port by name, never
    a number. `exec` is the general extension point.
58. No check means `process-only` readiness — named and shown wherever state
    appears.
59. Losing readiness removes an instance from traffic; only the `healthy` rule
    kills it.
60. `ready` thresholds are `successes` and `unready-failures`; `healthy` is
    `unhealthy-failures`.
61. The list of events that end the stabilization window is closed.

## Rollout and revisions

62. `replace` is `surge` or `swap`, declared not inferred; the default is `surge`.
63. A durable `exclusive` volume forces `swap`; a `shared` or `reset` volume allows
    `surge`. There is no `handover` field.
64. `floor` is the minimum ready count during a rollout, defaulting to
    `instances.min`. It applies only to `swap` — under `surge` the ready count
    never drops. `floor: 0` accepts downtime; all-at-once is one batch with
    `floor: 0`.
65. The plan reconciles `instances`, `replace`, volumes, and cluster capacity
    before the first change; surge capacity is rechecked before each batch and
    never assumed.
66. Surge is not an exception to `instances.max`.
67. Rollback restores a revision, never data; the plan states what it will not
    cover, and the mode is never inferred from a volume.
68. A failed rollout does not void the revision it created.
69. `batch.size`, `batch.percentage`, and `batch.partitions` are mutually
    exclusive.
70. The default strategy is `surge`, `batch 1`, with configurable `grace-period`,
    `readiness-timeout`, and `stabilization-period`.
71. Each deploy creates an immutable revision; the remote object is
    `metadata` / `spec` / `overrides` / `status`.

## Networking and endpoints

72. An application may have several named ports; a port's protocol is `http`/
    `https` (L7) or `tcp`/`udp` (L4), which decides whether the port can be
    multiplexed.
73. Visibility is `application`/`project`/`space`/`public`; the default is
    `project`.
74. An endpoint maps an external listener onto a named port, by name.
75. An ingress is a named traffic entry point declared in `cluster.yaml`, and is
    the uniqueness domain for endpoints.
76. The conflict key is `(ingress, port, hostname)` for http/https and
    `(ingress, port)` for tcp/udp; it contains neither project nor space.
77. The plan detects an endpoint conflict and names the claimant, even across
    spaces.
78. There is no path routing and no hostname-less catch-all endpoint.
79. `tls` appears only for `https`; its `mode` is `terminate` or `passthrough`.
    `http`, `tcp`, and `udp` have no `tls`.
80. The edge proxy picks a backend by hostname — the `Host` header for http, SNI
    for https — and after `terminate` connects per the target port's protocol.
81. A certificate is `automatic` (the installation issuer) or a secret; the edge
    proxy holds it, so renewal creates no revision and restarts nothing.
82. Chmura does not manage DNS; `chmura doctor` verifies that a hostname resolves
    to its ingress.
83. Egress is a separate policy.

## Service discovery and load balancing

84. An application is reachable inside Chmura by internal DNS: an app name
    (load-balanced across ready instances) within its visibility, plus a per-slot
    name for a specific instance. Internal DNS is separate from external and never
    leaves the installation.
85. Load balancing across ready instances is built in and always on — it is how
    endpoints (north–south) and internal names (east–west) reach an application at
    all. Health-aware, round-robin, optional session affinity.
86. Below a panic threshold of ready instances, the load balancer fails open —
    routing across every once-ready instance rather than to none.
87. Advanced traffic management (weighted/canary splitting, retries, circuit
    breaking) lives in the edge proxy as a future engine capability, absent in the
    first version.
88. The edge proxy terminates TLS, routes by host/SNI, and load-balances; it is
    realized by the engine and invisible in the user model.

## Storage and volumes

89. A volume's `allocation` is `shared` (one volume, concurrent, needs a
    `multi-attach` pool) or `exclusive` (one per slot). There is no separate
    `attachment` mode; a single-writer volume is `exclusive` with `instances: 1`.
90. For an `exclusive` volume, `size` is per instance, never a total.
91. `size` is optional; omitting it means an elastic volume — grown on a pool that
    supports it, otherwise an error asking for a size. A hard `max` needs a pool
    with the `quota` capability; otherwise `max` is advisory and monitored.
92. Shrinking a volume is not supported initially.
93. A slot stays stable across restart and deploy; the runtime ID is temporary.
94. Scale-down removes the highest-numbered slots (symmetry with reclaim); a
    vacated volume enters its lifecycle, not immediate destruction.
95. Slot numbering is `reuse` (default: compact, reclaimed) or `serial` (only
    increases, never reused, volumes not reclaimed).
96. Volume reclaim across scale-down and scale-up is best-effort; a slot's data is
    not guaranteed to survive.
97. `reset` is `never` (default, durable) or `on-deploy` (a fresh volume each
    deploy; the old one goes through cleanup, not in-place deletion).
98. Detached-volume lifecycle is `retain`, `expire`, or `pressure`; `pressure`
    cleanup is FIFO after a minimum age.

## Admin plane

99. The admin and user planes are one CLI and one API, split only by permission.
100. Nodes and capacity are reported by the agent; locations, pools, and policies
     are declared by an administrator.
101. `cluster.yaml` declares the engine, locations, ingress, and storage pools;
     `installation.yaml` declares storage policies (and the issuer, observability,
     identity, and secrets backends).
102. `chmura cluster apply` and `chmura installation apply` use the same machinery
     as a deploy.
103. A pool's `capacity` is a declared limit, not a measurement; the measurement is
     in status.
104. Pool `capabilities` are declared explicitly, never inferred from tags; the
     settled core is `multi-attach`, `resize`, `quota`, and `snapshots`.
105. Storage policies belong to the installation, not a cluster — that is their
     portability.
106. A project may tighten a policy's requirements, never relax them.
107. Pools, locations, ingresses, and policies have no create commands; the admin
     manifest changes them.
108. A cluster is created explicitly with `chmura cluster create`; a space names
     its cluster at creation.
109. There is no `chmura node add`; a node joins from its own side with a
     single-use token carrying its cluster and location.
110. Locations are Chmura's availability zones; multi-region is separate spaces
     plus external geo-DNS, with no cross-cluster data replication.

## Control plane and engine

111. The control plane is not an application on the Chmura it manages, and its host
     is not a node.
112. Control-plane redundancy is N stateless servers over a highly-available state
     database, not synchronized installations.
113. A control-plane outage does not stop running workloads; the agent reconciles
     the last assigned state.
114. The state database is the only non-recoverable element of an installation.
115. An installation is created by running `chmura-server`, not the CLI.
116. One execution engine is supported in the first versions; the engine is not a
     user choice, is uniform within a cluster, and is declared in `cluster.yaml`.
117. Engine differences surface only as capabilities and degradation, never as
     manifest fields; `chmura.yaml` never has an engine escape hatch.
118. Machine roles are control-plane host, node, workstation, and CI runner; CI is
     a distinct role, not a workstation variant.

## Communication and operations

119. The CLI talks only to the Chmura API.
120. The cluster agent initiates an outbound connection to the control plane.
121. The public API is HTTPS + JSON + SSE.
122. Long operations have an operation ID; state-changing requests carry an
     idempotency key.
123. `Ctrl+C` while watching does not cancel the remote operation.
124. A deploy's base revision is read in the same command, never from a local file;
     a mid-flight change by someone else is a conflict, not an overwrite.
125. The CLI authenticates with a token over HTTPS — interactively via
     `remote login` (token in the OS store, referenced in config) or via
     `CHMURA_TOKEN` in CI.
126. The identity backend is replaceable: built-in accounts or OIDC, set in
     `installation.yaml`; `remote login` is backend-independent.
127. A token carries identity; permissions are RBAC. Transport is HTTPS with
     explicit trust of the installation's certificate (pinned when self-signed).
     The agent authenticates separately with mTLS.

## Observability

128. Status is a snapshot; observability adds the time axis.
129. The signals are logs, events, metrics, and traces, split by who produces them.
130. Events are the domain timeline Chmura already produces, exposed addressably.
131. Platform metrics are always available; application metrics need
     `observability.metrics` and are scraped from a named port; no declaration is
     stated plainly.
132. Logs are captured automatically from stdout/stderr and forwarded verbatim;
     Chmura does not scrub secrets from them.
133. The observability interface is ours and the backend replaceable; the built-in
     store is bounded, and durable retention, dashboards, and alerting are an
     external sink in `installation.yaml`.
134. The CLI reads observability only through the API, never directly from the
     sink.

## Local dev

135. Local dev is an installation profile, not a separate engine — the same
     `chmura-server` and the same execution engine.
136. Local dev runs through `chmura-dev`; ordinary `chmura` commands work against
     the dev installation unchanged.
137. A dev installation is an ordinary remote; its location (localhost, VM, WSL)
     does not matter.
138. Deploying to a dev installation is allowed; the dev profile is reported and
     labeled but blocks nothing.
139. Dev differences are installation-policy relaxations, never manifest changes;
     the list is closed.
140. `chmura.dev.yaml` has its own schema, read only by `chmura-dev`, and does not
     change desired state; it may seed envelope-bounded dev overrides.
141. Dev values come from a literal, `from-env`, `from-file`, or `generate`; a
     committed `chmura.dev.yaml` never holds a secret value; a missing source stops
     `chmura-dev up`.
142. `chmura-dev up` is an explicit bootstrap; deploy still never creates spaces or
     projects, even in dev.
143. The default dev loop rebuilds and redeploys, creating revisions; hot
     source-sync (`reload`) is the only place an instance changes without a
     revision.
144. `chmura-dev` forwards named ports to `localhost` with the same mechanism as
     `chmura port-forward`.

## Conventions and boundaries

145. All Chmura-defined keys and enum values are kebab-case, like the CLI flags.
     Environment variable names (`UPPER_SNAKE`), chosen names, and external
     identifiers follow their own conventions.
146. Terraform is an infrastructure adapter, not the central application model.
147. Space migration is a future, multi-step operation; because a space name is
     unique within an installation, the address `remote:space` stays the same
     across a migration.
