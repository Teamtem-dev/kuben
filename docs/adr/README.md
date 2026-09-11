# Architecture decision records

Code comments cite decisions as `ADR-NNN`. This index resolves those numbers.
ADR-001 to ADR-012 were proposed in
[KUBEN-GOLDEN-ARCHITECTURE.md §16.2](../KUBEN-GOLDEN-ARCHITECTURE.md), and
ADR-013 to ADR-022 in the review
[KUBEN-ARCHITECTURE-CRITIQUE.md §6](../KUBEN-ARCHITECTURE-CRITIQUE.md), which
also records the reasoning. New decisions get their own file here
(template: Context, Decision, Consequences).

| ADR | Decision | Status | Where it shows in the code |
|---|---|---|---|
| 001 | Kubernetes holds desired state; SQL holds identity and audit | superseded by 015 | — |
| 002 | One binary with roles (modular monolith) | decided | `kuben serve --roles`, `kuben_core::config::Role` |
| 003 | Two Tokio runtimes as a bulkhead, supervisor, `panic = "unwind"` | superseded by 013 (supervisor and unwind kept) | `kuben_platform::supervise` |
| 004 | Projections (small read models) instead of raw reflectors | decided | `kuben_platform::projection` |
| 005 | Opaque cookie sessions and opaque API tokens; no JWT in the browser | decided | `kuben_api::auth` |
| 006 | sqlx with two backends (SQLite, PostgreSQL) and a test matrix | decided | `kuben-store`, `tests/matrix.rs` |
| 007 | OpenAPI-first: the TypeScript client is generated, CI fails on drift | decided | `kuben_api::openapi`, `packages/api-client` |
| 008 | Gateway API as the networking layer | decided | `controller::gateway` |
| 009 | BuildKit + Railpack/CNB, deploy by digest, immutable releases | superseded by 016 | — |
| 010 | CEL and ValidatingAdmissionPolicy instead of admission webhooks | decided | CRD schemas in `kuben-crd` |
| 011 | React 19 + TanStack for the UI | decided | `apps/web` |
| 012 | Licensing and a clean-room boundary to the system being replaced | decided | — |
| 013 | One runtime in phase 0; a second one only behind `runtime.bulkhead` | decided | `kuben::serve` |
| 014 | One SSE stream per browser tab; WebSocket only for terminals | decided | `kuben_api::stream` |
| 015 | Data ownership: SQL references CRDs only by `uid` | decided | `kuben-store`, Invariant I-17 |
| 016 | Persistent buildkitd plus a light `buildctl` Job; external registry in the MVP | decided, phase 1 | — |
| 017 | The binary applies its CRDs at boot (server-side apply) | decided | `controller::crd_apply` |
| 018 | Environments are soft-deleted with a grace period | decided | `controller::environment` |
| 019 | Explicit bootstrap order; polling fallback for webhooks | decided | `docs/deploy.md` |
| 020 | Append-only audit in the core; signed anchoring later | decided | `kuben_api::audit` |
| 021 | Six crates in phase 0, split only for a stated reason | decided | `Cargo.toml` |
| 022 | Threat model; Kuben's service account is treated like cluster-admin | decided, threat model pending | `charts/kuben/templates/rbac.yaml` |
| 023 | [Lease-based leader election for the controllers](0023-controller-leader-election.md) | decided | `kuben_platform::leader` |
