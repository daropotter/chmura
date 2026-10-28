# Manifest validation

`internal/manifest` holds the shared project model and YAML parser. It is a
building block for the CLI and control plane; there is no validation command or
deployment endpoint yet.

The initial implementation covers `version: 1`, an explicit project `name`, and
one or more named `applications`. Each application selects exactly one source:

- A prebuilt image: `source: {image: ghcr.io/acme/api:1.0}`.
- A Dockerfile build: `source: {context: ., dockerfile: Dockerfile}`.

Both source forms are parsed, including the form emitted by `chmura init`.
Parsing does not check local paths, validate registry image-reference syntax,
contact a registry or build an image. Dockerfile execution remains a later
milestone.

The parser requires a single YAML document, rejects duplicate keys and unknown
fields, and checks scalar types without coercion. It preserves explicit names
and paths rather than deriving or normalizing them. Source forms cannot be
combined, and a Dockerfile source requires both paths.

Applications may declare named `ports`. Numbers must be integers from 1 to
65535, and protocols are `http`, `https`, `tcp` or `udp`. Visibility is
`application`, `project`, `space` or `public`; omitting it resolves to `project`
in the returned model. Multiple ports are allowed and names are preserved.

The `health` model keeps readiness (`check` and `ready`) independent of the
opt-in restart rule (`healthy`). Each explicit probe selects exactly one of
`http`, `tcp`, `process` or `exec`. HTTP and TCP probes refer to a declared port
on the same application; HTTP requires an HTTP/HTTPS port, and TCP accepts any
TCP-based protocol. UDP ports cannot be used by these probes.

Omitting `health` preserves a nil configuration. Omitting `check` means
process-only readiness. Readiness thresholds default independently to one
success and three failures. A `healthy` rule requires a positive failure
threshold and never inherits the readiness probe; without its own `check`, it
watches only the process. `exec.command` is an argument list, with a non-empty
executable; later arguments may be empty strings.

Explicit probe intervals, timeouts and startup timeouts must be positive Go
duration strings (for example `5s` or `2m`). Omitted timings remain unspecified
(zero in the Go model); the product specification has not set their defaults.
An omitted HTTP `expect-status` also remains unspecified. This increment
validates declarations only; it does not execute probes or restart processes.

This is a subset of the [product schema](../reference/manifest.md). Other fields
(including resources, environment, volumes and endpoints) are
rejected until their models and validation land in later increments. They are
never silently ignored. Deployment targets and engine-specific fields have no
place in the portable manifest.
