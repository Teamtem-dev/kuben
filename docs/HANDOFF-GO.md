# Kuben Go rewrite — handoff (2026-09-23)

Repo: `/Users/fa/Desktop/kubex/kuben-monorepo` · remote `github.com/Teamtem-dev/kuben` · product version 1.2.0 (Rust, released)
Plan (source of truth, Persian): `docs/KUBEN-GO-REWRITE-PLAN.md` (v2.1, approved by the owner on 2026-09-21). `docs/` is local and gitignored.

## 0. Rules from the owner (binding)

- **Never `git push`** or touch a remote (no PRs, no tags). Commit locally only. The owner pushes.
- Replies to the owner in **Persian**; code, comments, logs, commit messages in **English**.
- Commits: Conventional Commits, body explains why, last line `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>` (use the model attribution the system reminder gives).
- No heavy local browser/Playwright runs (CI runs them).
- Quality over binary size: size budgets are warnings only; correctness, standard libraries and clean code first.
- Release train: 2.0.0-alpha.N → beta.N → rc.N (7 days, no P0/P1) → 2.0.0 (same bytes as last rc). **2.0.0 adds no DB migration** (first new migration is in 2.1.0) so rollback to 1.2.x is an image swap.
- Rust is feature-frozen until cutover (bug fixes only).

## 1. Project and stack

Kuben = self-hosted Kubernetes PaaS (Dokploy-like UX). Monorepo (Bun + Turborepo 2.11.2):

- `crates/*` — current Rust product (kuben, kuben-api, kuben-store, kuben-platform, kuben-core, kuben-crd, kuben-agent). Frozen; deleted at phase G7.
- `go/` — the rewrite. `go.work` modules:
  - `go/hub` (module `github.com/Teamtem-dev/kuben/go/hub`, the `kuben` binary; Turborepo package `hub`)
  - `go/agent` (cluster agent, skeleton only), `go/kubenapi` (CRD types + future hub↔agent protocol), `go/tools` (pinned `go tool`s)
  - Go 1.27 (toolchain go1.27.1). Hub and agent `require` kubenapi with `replace ../kubenapi`.
- `apps/console` — React 19 / Vite 8 / Tailwind 4 / TanStack; `apps/site` — Astro docs; `packages/api-client/openapi.json` — **frozen API contract** (OpenAPI 3.1, 129 ops).
- `charts/kuben` — Helm chart; `charts/kuben/crds/kuben.dev_all.yaml` — **frozen CRD manifest**.

Go stack (decided, see `go/SPIKES.md`, `go/SUBSTITUTIONS.md`): net/http; **ogen** server generated from the contract (server only, no OTel); pgx v5 (no ORM, no sqlc — SQL copied verbatim from Rust); own migrator on sqlx's `_sqlx_migrations` table; client-go v0.37 + controller-runtime v0.25.1; controller-gen (deepcopy only); cobra; koanf; slog; argon2id (alexedwards); golang-lru expirable.

Must-read docs in repo: `go/CONVENTIONS.md` (binding rules), `go/PARITY.md` (Rust file → Go status), `go/SUBSTITUTIONS.md` (every library swap + known differences), `go/SPIKES.md`.

## 2. Environment gotchas (this Mac's sandbox)

- Before any Go command: `. /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/env.sh` (GOPROXY = `file://…/.cache/go/proxy` mirror, GOSUMDB off, GOTOOLCHAIN go1.27.1, caches in `$TMPDIR`). `/.cache` is gitignored.
- Go's own TLS cannot reach the sandbox proxy → modules come from the mirror, filled by curl: `python3 .cache/go/gofetch.py get MOD[@VER]`, `… graph MOD@VER` (prefetch .mod graph), `… loop -- <go command>` (fetch whatever the command reports missing, repeat). Pass `allowed_domains ["proxy.golang.org","storage.googleapis.com"]`.
- `$TMPDIR` is wiped between sessions (module/build caches re-extract from the mirror; the mirror in `.cache/go/proxy` survives).
- **PostgreSQL cannot run in the sandbox** (SysV shm blocked) → DB tests skip locally; CI sets `KUBEN_TEST_PG_URL` + `KUBEN_REQUIRE_PG=1` (a skip fails). Nothing DB-backed has run yet.
- golangci-lint and staticcheck cannot be built locally (sandbox blocks writing `.vscode`/`.gitmodules` from module zips) → CI runs golangci-lint v2.13.2 via the official action. Locally: `bash ../../scripts/go-check.sh` (gofumpt, vet, exhaustive, go-check-sumtype, NilAway) from a module dir, plus `go test -race ./...`.
- Bun: `BUN_TMPDIR=$TMPDIR/bun BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache`; installs must use `BUN_CONFIG_REGISTRY=https://registry.npmjs.org/` so `bun.lock` stays registry-neutral (user's global registry is npmmirror; `grep -c npmmirror bun.lock` must be 0).
- Rust: `export CARGO_HOME=/tmp/claude/cargo-home CARGO_TARGET_DIR=$PWD/target CARGO_INCREMENTAL=0`; `cargo … --offline --locked` (fetch with allowed_domains index.crates.io/static.crates.io if needed).
- `git checkout -b X origin/main` silently fails here (tracking config) → use `git checkout --no-track -b X <sha>`.
- Never run `go mod tidy`/`go get` while a background agent edits the same module (it once dropped pgx).

## 3. Branches and worktrees (all local, nothing pushed)

| Branch | Where | Base | State |
|---|---|---|---|
| `feat/go-rewrite` | main checkout | `origin/main` 86ce940 | G0, G1, G2, S1-A…D and part of S1-E committed (head `c9421b5`); clean tree |
| `feat/console-shadcn` | worktree `.cache/wt/console-shadcn` | 86ce940 | F0 done (5 commits); planned release as 1.3.0 on the Rust backend |
| `fix/crd-yaml11-booleans` | worktree `.cache/wt/hotfix` | 86ce940 | 1 commit `eee4ece`: hotfix for released 1.2.0 (candidate 1.2.1) |

## 4. Done

**G0 (foundation)**: go.work, CONVENTIONS/PARITY/SUBSTITUTIONS, `.golangci.yml`, `go/tools` (nilaway, exhaustive, go-check-sumtype, errcheck, gofumpt, govulncheck, ogen, controller-gen), Turborepo Go workspaces (`test` = `go test -race -shuffle=on`, `lint` = `scripts/go-check.sh`, `hub#build` = `scripts/go-build.sh`), CI jobs `go` (golangci action, race tests on PG17, govulncheck, `go mod verify`, `scripts/rust-drift.sh`) and `oracle` (downloads released Rust v1.2.0, runs both servers on empty DBs, compares `go/hub/test/oracle` scenario). Spikes: ogen chosen (oapi-codegen does not compile on the 3.1 spec); hub deps ≈58 MiB, agent 25 MiB with standard discovery. `.goreleaser.yaml` (5 targets, prerelease never latest).

**G1 (core)**: all 23 `kuben-core` modules → `go/hub/internal/core/*` (26 packages); every Rust test ported + JSON pins; state machines as transition tables with exhaustive pair tests; semver/glob/IDNA ported semantically (library behaviour differed); config = koanf + ported figment env grammar; `config.Secret` redaction.

**Compatibility fixtures** (Rust `#[ignore]` generators `crates/kuben-platform/src/compat_fixtures.rs`, `crates/kuben-api/tests/compat_fixtures.rs` → `go/hub/testdata/compat/*.json`): canonical JSON, 4000 floats, password PHC, tokens, session hash, webhook signatures, sealed secrets. `internal/wire.Canonical` is byte-identical to Rust `render::canonical` (ports serde_json's non-correctly-rounded number parser and the literal 1e0…1e308 table).

**G2 (walking skeleton)**: store foundation (sqlx-compatible migrator on `_sqlx_migrations`, SHA-384, same advisory lock; RLS `set_config('kuben.org_id',…)`; users/sessions/orgs/throttle/audit/tokens); API: setup (token file, secure transport), login (3-bucket throttle), logout, me, change password, SSO info (disabled), projects list/get, health, audit middleware (operation id from ogen `FindRoute`), SSE stream (empty source), web console embed with strict CSP, problems byte-exact; `kuben serve|setup-token|version`; binary 15 MiB built in 12 s.

**S1 so far (committed)**: kubenapi v1alpha1 (8 CRDs, generated DeepCopy, structural test vs manifest, 34 round-trips vs real Rust output); store for operations, lifecycle, resolve, catalog, releases, deployments, materialize, capabilities, retention, support (+ partial agents/policies/controls/previews/secrets/scans); platform supervise, leader (client-go leaderelection, Lease `kuben-controller` 15/10/2 s, release on cancel), registry; API tokens, members, audit list, scoped access, scope.go; auth **gate** (401/403 before decoding, like axum extractor order).

**Bugs found and fixed**:
- CRD manifest wrote `off` unquoted → YAML 1.1 (kubectl/Helm) read `false` for `idle.mode` default/enum. Fixed in Rust `kuben_crd::manifest_yaml` (quotes YAML 1.1 booleans); on both `feat/go-rewrite` (0f27d62) and hotfix branch.
- ogen dropped free-form `{type: object}` members (deployment `config`, export, Doctor graph) → `internal/api/genspec` makes `additionalProperties: true` explicit before generation (contract file unchanged).
- JSON generic decoding to float64 turned `1.0` into `1` (hash drift) → `wire.DecodeAny` (UseNumber) mandatory; store switched.
- `kerr.Wrap(nil)` panicked → nil-safe. bun.lock had registry URLs baked in → regenerated neutral.

**F0 (console, branch feat/console-shadcn)**: 61 raw shadcn components in `src/components/ui/` (patches listed in `PATCHES.md`: RTL codemod, chart CSSOM, progress RTL, cn import), all tokens in `apps/console/src/styles/theme.css`, new shell (sidebar, breadcrumb, ⌘K, theme, language), zero CSP violations without `unsafe-inline` (style-singleton via adoptedStyleSheets + build-time CSS extraction plugin), lazy routes (initial JS 162/200 kB brotli).

## 5. S1-D done (2026-09-23)

Committed: platform/render, projection, discovery (NilAway fixed), serve wiring (informers → readiness, discovery, materializer worker on every controller replica, reconcilers + drift watch under the Lease), platform/controller (controller-runtime; 16 Rust tests → 34), platform/materializer (20 Rust tests → 17; the 3 secrets tests wait for the keyring), store reads (pause, delivery, observation, detach.rs), kubenapi `DecodeAppSpec` (serde-strict required members) and `protocol.Apply`. Full `go-check.sh` + `go test -race ./...` green.

Known gaps, recorded in PARITY: materializer secrets need `secrets.rs` keyring (S2) → runs bound to secrets fail `SecretsUnavailable` like Rust without a keyring; tests/materializer.rs and all reconciler behaviour against a real API server need envtest (CI); `kuben_reconcile_errors_total` not emitted (no metrics registry yet).

## 6. Pending (priority order)

1. ~~NilAway / render, projection, discovery~~ · ~~serve wiring~~ · ~~S1-D materializer + controllers~~ (done, see §5).
4. S1-E: API routes for projects create/delete, environments, apps (crud, spec, deployments, releases, promote, logs, jobs, domains, approvals, admission, image policy later), templates; extend `test/oracle` scenarios.
5. Finish S1 exit: all `m1_*` scenarios from `crates/kuben-api/tests/http.rs` ported; oracle green in CI (owner must push or run locally with PostgreSQL).
6. Then S2 (OCI, domains/Gateway/certs, doctor, **agent + AgentLink** mTLS frame protocol, registry logins, secrets keyring), S3 (Git/BuildKit), S4 (policy/controls/incidents/webhooks/backup/upgrade/detach/SSO/CI), S5 (previews/DNS/status/image policy/usage/evidence), G5 (CLI/bootstrap/upgrade/backup), G6 release train, G7 delete Rust. Console F1–F3 in parallel on `feat/console-shadcn`.

## 7. Verification commands

```bash
. /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/env.sh
cd /Users/fa/Desktop/kubex/kuben-monorepo/go/hub && go build ./... && go vet ./... && go test -race ./... && bash ../../scripts/go-check.sh
cd ../kubenapi && go test -race ./... && bash ../../scripts/go-check.sh
cd ../.. && scripts/rust-drift.sh && bash scripts/ci-changes.test.sh
bun turbo run build --filter=hub   # builds go/hub/bin/kuben with the console embedded
```
Owner-side (outside the sandbox, needs Docker): `docker run -d --name kuben-pg -e POSTGRES_PASSWORD=kuben -p 5432:5432 postgres:17-alpine`, then `cd go/hub && KUBEN_TEST_PG_URL=postgres://postgres:kuben@localhost:5432/postgres KUBEN_REQUIRE_PG=1 go test -race ./...`.

## 8. Immediate next action

S1-E so far (2026-09-23): projects create/delete, environments, apps (crud, restart, handover), deployments (start with Idempotency-Key, list, get), releases and rollback, api/oci (go-containerregistry). Git-sourced apps answer 501 until S3; registry logins wait for the keyring (S2); a registry 429 answers 503 as in Rust.

Next: port the remaining S1 app routes from `crates/kuben-api/src/routes/apps/` — `promote.rs`, `jobs.rs`, `logs.rs` (needs the cluster; `environments_and_apps_read_from_sql` expects 503 for logs without one), `domains.rs`, `approvals.rs` (decide/list) — each with its tests, then templates, then extend `go/hub/test/oracle` scenarios. Report in Persian after each route group.
