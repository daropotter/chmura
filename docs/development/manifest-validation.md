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

This is a subset of the [product schema](../reference/manifest.md). Other fields
(including resources, ports, health, environment, volumes and endpoints) are
rejected until their models and validation land in later increments. They are
never silently ignored. Deployment targets and engine-specific fields have no
place in the portable manifest.
