# Storage & volumes

!!! note "Draft"
    This page is being written. The outline below is the planned structure.

A **volume** is a logical, durable resource with a stable identity, independent
of the instance that uses it.

- **Allocation & attachment** — `shared` vs `per-instance`, `exclusive` vs
  `concurrent`, and how a per-instance volume binds to a stable slot.
- **Sizing** — `min`/`preferred`/`max`; an omitted `size` means an elastic volume
  that grows on demand.
- **Slot binding, reclaim, and `reset`** — how volumes follow slots across
  restarts and deploys, what scale-down and scale-up do, and when a volume starts
  fresh.
- **Lifecycle & retention** — `retain`, `expire`, `pressure`; protection; and
  orphaned volumes after a project is deleted.
- **Policies & pools** — how a project's portable storage policy is matched to a
  cluster's declared pools (see also [Architecture](architecture.md)).
