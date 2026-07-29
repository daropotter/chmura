# Networking & endpoints

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

- **Named ports and visibility** — an application's ports, and who can reach them
  (`application`, `project`, `space`, `public`).
- **Endpoints and port mapping** — exposing a named port to the outside world.
- **Ingress** — the cluster-declared entry points for traffic.
- **Uniqueness and conflicts** — `(ingress, port, hostname)` for HTTP, `(ingress,
  port)` for raw TCP/UDP; wildcards; no catch-all.
- **TLS** — `terminate` vs `passthrough`, who holds the certificate, and how the
  backend is chosen (Host header vs SNI).
- **Request path and certificate lifecycle** — what happens when a request
  arrives, and how `automatic` and secret-backed certificates are issued and
  renewed.
- **Firewall and egress** — inbound rules derived from declarations; egress as a
  separate policy.
