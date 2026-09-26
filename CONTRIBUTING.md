# Contributing to Kuben

Thanks for helping. This file covers the workflow and the review checklist:
the 18 invariants every change must keep.

## Workflow

1. `bun run setup` once, then `bun run dev` (API on `:8080`, Vite on `:5173`).
   The API needs a PostgreSQL in `KUBEN_DATABASE__URL`; the
   [development setup](https://kuben.teamtem.com/docs/contributing/development/)
   starts one with `docker run`.
2. Keep changes focused; one concern per pull request.
3. Before pushing: `bun run ci` (gofumpt, go vet and the static checks of
   `scripts/go-check.sh`, `go test -race`, drift check, govulncheck, Biome,
   typecheck, web test/build/size). If you touched controllers or the API, also
   run `bun run e2e` against kind.
4. Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
   (`feat(api): …`, `fix(controller): …`). Label PRs (`feature`, `bug`,
   `security`, `breaking`) — release notes are grouped by label.

Every task runs through Turborepo (`turbo.json`). Root `package.json` scripts
are the entry points: `bun run build | check | lint | format | test | gen`.
The Go modules are Turborepo packages like the web ones (`kuben`,
`kuben-agent`, `api`) with the same task names: `bun turbo run test
--filter=kuben`, `lint --filter=api`, `build --filter=kuben-agent`. See
ADR-024 (how the task graph is organised) and ADR-033 (the Go runtime) in the
[architecture decisions](https://kuben.teamtem.com/docs/contributing/architecture-decisions/). Go code follows
[`apps/kuben/CONVENTIONS.md`](apps/kuben/CONVENTIONS.md);
[`ARCHITECTURE.md`](ARCHITECTURE.md) maps the repository, its packages and
the dependency rules.

Repository rules:

- **Dependencies:** Go versions live in the `go.mod` of each module of
  `go.work` (`apps/kuben`, `apps/kuben-agent`, `packages/api`; the pinned
  tools in `tools/go.mod`, outside the workspace); JS versions only in
  `workspaces.catalog` of the root `package.json` (packages say `catalog:`),
  and the TypeScript settings in `@kuben/typescript-config`. A new Go
  dependency needs a reason in the PR, and govulncheck must stay clean.
- **The contracts are frozen:** `packages/api-client/openapi.json` (the REST
  API), `charts/kuben/crds/kuben.dev_all.yaml` (the CRDs) and the SQL schema
  are the ones 1.2 shipped. The Go server is generated from the spec (ogen,
  `bun turbo run gen --filter=kuben`) and the Go tests pin the embedded
  CRDs to the manifest. Changing a contract is a deliberate change of that
  file, then `bun run gen` for the TypeScript types; CI fails on drift and on
  a breaking API change without the `breaking` label.
- **Every operation has a unique `operationId`** in the spec; the TypeScript
  types and the generated Go handlers are keyed by it.
- **CRD types must stay structural:** no internally tagged unions (use
  one-of structs with optional fields, like `Source { image, git }`).
- **Budgets are gates:** binary ≤ 45 MiB, image ≤ 50 MiB
  (`scripts/check-budgets.sh`), web JS ≤ 200 kB brotli. Raising a budget needs
  a written reason in the PR.

## Review checklist — the 18 invariants

Each invariant closes a class of bugs found in the system Kuben replaces.
Reviewers check the ones a change touches. Paths are Go packages under
`apps/kuben/internal/` unless they say otherwise.

| # | Invariant | Where it is enforced |
|---|---|---|
| I-1 | No mutation or subscription without an authorization `Proof` from the policy engine; the SSE stream is filtered per org. | `core/authz`, `httpapi/stream` |
| I-2 | No default secrets. Keys and the admin password are configured or generated on first boot; missing key → fail closed. | `firstrun` |
| I-3 | Browser sessions are opaque `__Host-` cookies (`HttpOnly; Secure; SameSite=Lax`), never JWTs in JS; strict CSP without inline scripts. | `httpapi/auth` (session), `httpapi/web` (`web.CSP`) |
| I-4 | Passwords: Argon2id only, constant-time comparison, dummy hash for unknown users. | `httpapi/auth` (password), the login route |
| I-5 | Same-origin by default; mutations need the `x-kuben-client` header and a same-origin `Sec-Fetch-Site`; WebSocket upgrades check `Origin`. | `httpapi/httpx` |
| I-6 | No injection: names are validated as DNS labels and resolved from projections, never interpolated from user strings into queries. | `httpapi` (`validate.go`, `scope.go`) |
| I-7 | Build pods never see the API server: no service-account token, egress limited to git/registry, helper images pinned by digest. | `build` (`job.go`: no service-account token). Egress limits are the cluster's; `kuben serve` warns about build images not pinned by digest |
| I-8 | WebSocket authentication happens at upgrade; topics are authorized. | terminals (not built yet) |
| I-9 | Terminal content is never logged; only metadata goes to the audit log. | terminals (not built yet) |
| I-10 | Templates never ship fixed passwords (`generate: password`). | `httpapi` (`templates.go`) |
| I-11 | No global mutable "current context": an immutable cluster registry, explicit cluster ids. | `kube/registry` |
| I-12 | Log streaming is bounded: ref-counted hub, bounded channels, maximum line length, drop-with-marker. | `httpapi` (`apps_logs.go`): bounded reads, and followed logs capped per user and per replica, an hour at most |
| I-13 | One shell session per tab and user, with an idle timeout. | terminals (not built yet) |
| I-14 | Informers and projections; never poll full lists on a timer. | `kube/projection` |
| I-15 | Ready only after the informers synced; ordered graceful shutdown: readiness off → drain → cancel subsystems (the leader releases its Lease) → flush → checkpoint. | `server`, `kube/leader` |
| I-16 | Embedded migrations under an advisory lock, with the checksums sqlx recorded; foreign keys always on. | `store/migrations`, `store/migrate` |
| I-17 | One source of truth per datum: CRDs hold desired state, SQL holds identity and audit; SQL references CRDs only by `uid`. | `packages/api/v1alpha1`, `store` |
| I-18 | Controllers use typed builders, server-side apply (field manager `kuben`), kstatus conditions and `observedGeneration`. | `kube/render`, `kube/controller` |

Also check:

- Templates, console text and docs are written from upstream documentation,
  never copied from the system Kuben replaces (ADR-012).
- Errors returned to clients never contain internal details (`kerrors.Internal` → no `detail`).
- Objects in another org answer `404`, not `403`.
- New long-running tasks run under `supervise` and report into `health`.
- Work that must happen once per cluster (reconciling, applying CRDs) runs
  under the controller Lease, never on every replica (ADR-023).
- Migrations are PostgreSQL only (ADR-025), in
  `apps/kuben/internal/store/migrations/`. An applied migration is never edited,
  not even a comment: the checksum of every file is recorded and existing
  databases would refuse to start. 2.0 adds no migration (rollback to 1.2 is
  an image change); the next one is `0035` in 2.1.
- Pure logic (builders, validation, parsing) has unit tests; API behaviour has
  HTTP tests in `apps/kuben/internal/httpapi` (PostgreSQL, run in CI); cluster
  behaviour is covered by envtest, the kind jobs and `scripts/e2e.sh`.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Please do not open public issues for vulnerabilities.
