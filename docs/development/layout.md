# Repository layout

A monorepo: one module, one `go.mod`, the binaries as `cmd/<artifact>`. The
inside of each module is left open for now — we don't decompose ahead of the
architecture; it fills in as the code lands.

```text
chmura/
├── cmd/                      # thin binary entrypoints (main.go + flags)
│   ├── chmura/               # the CLI (primary interface)
│   ├── chmura-server/        # control plane + API + state DB (stateless)
│   ├── chmura-agent/         # cluster agent (dials out, mTLS, reconcile)
│   └── chmura-dev/           # local development profile
│
├── internal/                 # all private logic (Go's import boundary)
│
├── api/                      # contracts (placeholder; arrives with need)
│   └── proto/                # gRPC agent ↔ server (mTLS)
│
├── docs/                     # the public specification and these dev guides
│
├── go.mod / go.sum
├── Makefile                  # build/test/vet for all artifacts
└── .gitignore
```

## Layout rules

- `cmd/<artifact>/main.go` is **thin** — it parses flags and calls into
  `internal/`.
- All logic lives under `internal/` (the private import boundary). Reach for
  `pkg/` only when something is meant to be imported by external projects — not
  yet.
- Whatever is shared between binaries (the model, contracts, version, PKI) also
  lives under `internal/`.
- `api/proto/` appears with the first gRPC contract — we don't create an empty
  one.

## Why a monorepo

The four artifacts share types, contracts, and logic (the model, the API, PKI). A
monorepo gives them one versioning story for those contracts, one pipeline, and no
drift between binaries. See the [engineering decisions](decisions.md).
