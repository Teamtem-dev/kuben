# Contributing to Kuben

Thanks for helping. This file covers the workflow and the review checklist:
the 18 invariants every change must keep.

## Workflow

1. `bun run setup` once, then `bun run dev` (API on `:8080`, Vite on `:5173`).
2. Keep changes focused; one concern per pull request.
3. Before pushing: `bun run ci` (fmt, clippy `-D warnings`, tests, drift check,
   Biome, typecheck, web test/build/size). If you touched controllers or the API,
   also run `bun run e2e` against kind.
4. Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
   (`feat(api): …`, `fix(controller): …`). Label PRs (`feature`, `bug`,
   `security`, `breaking`) — release notes are grouped by label.

Every task runs through Turborepo (`turbo.json`). Root `package.json` scripts
are the entry points: `bun run build | check | lint | format | test | gen`.
One package works too: `bun turbo run test --filter=kuben-store`. See
[the migration plan](docs/turborepo-migration-plan.md) for the full task table.

Repository rules:

- **Dependencies:** Rust versions live only in `[workspace.dependencies]`;
  JS versions only in `workspaces.catalog` of the root `package.json`
  (packages say `catalog:`). Every new crate must pass `cargo deny check`.
- **Generated files are committed:** after changing API types or CRDs run
  `bun run gen` and commit `packages/api-client/openapi.json`,
  `packages/api-client/src/schema.d.ts` and `charts/kuben/crds/`. CI fails on
  drift.
- **Every handler gets a unique `operation_id`** in `#[utoipa::path]`; the
  TypeScript types are keyed by it.
- **CRD types must stay structural:** no internally tagged enums (use
  one-of structs with optional fields, like `Source { image, git }`).
  `all_crds_have_structural_schema` guards this.
- **Budgets are gates:** binary ≤ 26 MiB, image ≤ 30 MiB, web JS ≤ 200 kB
  brotli. Raising a budget needs a written reason in the PR.

## Review checklist — the 18 invariants

Each invariant closes a class of bugs found in the system Kuben replaces.
Reviewers check the ones a change touches.

| # | Invariant | Where it is enforced |
|---|---|---|
| I-1 | No mutation or subscription without an `AuthzProof` from the policy engine; the SSE stream is filtered per org. | `kuben_core::authz`, `Authz::require`, `stream::Visibility` |
| I-2 | No default secrets. Keys and the admin password are configured or generated on first boot; missing key → fail closed. | `kuben::bootstrap` |
| I-3 | Browser sessions are opaque `__Host-` cookies (`HttpOnly; Secure; SameSite=Lax`), never JWTs in JS; strict CSP without inline scripts. | `auth::session`, `web::CSP` |
| I-4 | Passwords: Argon2id only, constant-time comparison, dummy hash for unknown users. | `auth::password`, `auth::login` |
| I-5 | Same-origin by default; mutations need the `x-kuben-client` header and a same-origin `Sec-Fetch-Site`; WebSocket upgrades check `Origin`. | `auth::csrf_guard` |
| I-6 | No injection: names are validated as DNS labels and resolved from projections, never interpolated from user strings into queries. | `routes::validate`, `routes::scope` |
| I-7 | Build pods never see the API server: no service-account token, egress limited to git/registry, helper images pinned by digest. | build controller (phase 1) |
| I-8 | WebSocket authentication happens at upgrade; topics are authorized. | terminal (phase 1) |
| I-9 | Terminal content is never logged; only metadata goes to the audit log. | terminal (phase 1) |
| I-10 | Templates never ship fixed passwords (`generate: password`). | template catalog (phase 2) |
| I-11 | No global mutable "current context": an immutable `ClusterRegistry`, explicit cluster ids. | `kuben_platform::registry` |
| I-12 | Log streaming is bounded: ref-counted hub, bounded channels, maximum line length, drop-with-marker. | `LogHub` (phase 1); today logs are bounded REST reads |
| I-13 | One shell session per tab and user, with an idle timeout. | terminal (phase 1) |
| I-14 | Informers and projections; never poll full lists on a timer. | `projection::informer` |
| I-15 | Ready only after the informers synced; ordered graceful shutdown: readiness off → drain → cancel subsystems (the leader releases its Lease) → flush → checkpoint. | `kuben::serve`, `kuben_platform::leader` |
| I-16 | Embedded migrations (`sqlx::migrate!`) under a lock; foreign keys always on. | `kuben_store::db` |
| I-17 | One source of truth per datum: CRDs hold desired state, SQL holds identity and audit; SQL references CRDs only by `uid`. | `kuben-crd`, `kuben-store` |
| I-18 | Controllers use typed builders, server-side apply (field manager `kuben`), kstatus conditions and `observedGeneration`. | `controller::resources`, `controller::*` |

Also check:

- Templates, console text and docs are written from upstream documentation,
  never copied from the system Kuben replaces (ADR-012).
- Errors returned to clients never contain internal details (`Error::Internal` → no `detail`).
- Objects in another org answer `404`, not `403`.
- New long-running tasks run under `supervise` and report into `Health`.
- Work that must happen once per cluster (reconciling, applying CRDs) runs
  under the controller Lease, never on every replica (ADR-023).
- Each migration exists for both backends with the same file name;
  `schema_parity_between_sqlite_and_postgres` enforces it.
- Pure logic (builders, validation, parsing) has unit tests; API behaviour has
  tests in `crates/kuben-api/tests/http.rs`; cluster behaviour is covered by
  `scripts/e2e.sh`.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Please do not open public issues for vulnerabilities.
