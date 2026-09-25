# Kuben Go rewrite — handoff (2026-09-25)

Repo: `/Users/fa/Desktop/kubex/kuben-monorepo` · remote `github.com/Teamtem-dev/kuben` · product version 1.2.0 (Rust, released)
Plan (source of truth, Persian): `docs/KUBEN-GO-REWRITE-PLAN.md` (v2.1, approved by the owner on 2026-09-21). `docs/` is local and gitignored.

## 0. Rules from the owner (binding)

- Since 2026-09-25 the owner pushes `feat/go-rewrite` to GitHub and has the agent continue **on that branch** (commit and `git push origin feat/go-rewrite`). Still: no PRs, no tags, no other branches unless the owner asks.
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

### 2b. Cloud container (Claude Code on the web, from 2026-09-25)

- Checkout `/home/user/kuben`; Go 1.27 downloads itself; modules come straight from proxy.golang.org (no mirror, no env.sh).
- PostgreSQL 16 is installed: `service postgresql start`, password `kuben` for `postgres`. Every database test runs: `KUBEN_TEST_PG_URL=postgres://postgres:kuben@localhost:5432/postgres KUBEN_REQUIRE_PG=1 go test -race ./...`.
- golangci-lint v2.13.2 builds here: `GOTOOLCHAIN=go1.27.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2`, then `~/go/bin/golangci-lint run --allow-parallel-runners --config ../../.golangci.yml ./...` from `go/hub`.
- NilAway needs `-include-pkgs github.com/Teamtem-dev/kuben` (in scripts/go-check.sh): without it the analysis of client-go takes >13 GiB and is killed.
- Bun: the repo pins bun 1.4.2 (`packageManager`); the preinstalled 1.3 refuses the lockfile. Fetch `https://github.com/oven-sh/bun/releases/download/bun-v1.4.2/bun-linux-x64.zip` and run `BUN_CONFIG_REGISTRY=https://registry.npmjs.org/ bun install --frozen-lockfile`, then `bun run build` in apps/console.
- The oracle runs locally: download `kuben-x86_64-unknown-linux-musl.tar.gz` of release v1.2.0, build the Go binary with `scripts/go-build.sh`, start both with `KUBEN_DATABASE__URL`, `KUBEN_SERVER__BIND=127.0.0.1:300{1,2}`, `KUBEN_SERVER__STATE_DIR`, then `KUBEN_ORACLE_RUST=… KUBEN_ORACLE_GO=… go test -run TestSkeletonMatchesRust ./test/oracle/`.
- `go.work.sum` changes whenever a tool is `go run`; restore it (`git checkout go.work.sum`) before committing.
- CI's `go` and `oracle` jobs run only on pull requests (or workflow_dispatch); a push to the branch alone runs nothing.

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

## 4b. Review and S1 closure (2026-09-25)

- First run of the database tests against PostgreSQL: 13 failures. Real bugs fixed: the audit middleware recorded every request as `success` (it read the status through http.TimeoutHandler's writer), audit `status` was always null (json.Number read as float64), an invalid policy answered 500. The rest were test defects (fixed to follow tests/http.rs).
- golangci-lint (never run before): 442 findings → 0 in go/hub and go/kubenapi. Tuned: govet shadow off, gocyclo 20, test exclusions, recvcheck skips DeepCopy.
- The S1-E routes written after c9421b5 were reviewed line by line against Rust and fixed: logs (long lines, stable sort, fractional times, errgroup, clock, opt.Val, SSE bytes), promote/jobs/domains (project deleting, IpAddr order, DNS timeouts and concurrency, empty process, Artifact sum type, platform/doctor, platform/secrets.SecretID), approvals/policy (explicit nulls, store error, plan hash via policy.Unhex, error texts, saturating counts), templates/status (stable sort, store clock, orphan cleanup, catalogue as a function). testify is gone.
- New: `/api/docs` (Scalar page over the frozen contract, api/apidocs), gzip compression as tower-http's CompressionLayer (httpx.Compressor, SUBSTITUTIONS row), axum's keep-alive bytes.
- The oracle ran for the first time and found contract drift, fixed: `application/json` without charset, console types from mime_guess, CSRF by header presence. Its S1 scenario now covers apps, deployments, releases, logs without a cluster and members; it passes against the released 1.2.0.
- tests/http.rs: 27 of 44 ported (the rest belong to S2–S5).

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

## 8. Status (2026-09-25, end of session) and next action

Branch `feat/go-rewrite` (local commits, **not pushed**: the owner pushes and merges). Ledger: go/PARITY.md — 231 Rust files: ported 227, dropped 4, todo 0, partial 0. Rust is removed from the branch (G7).

**Important:** at the owner's request ("don't build, just write the code") everything merged after commit `0c26460` was written **without compiling, vetting, linting or testing** (the agents' earlier parts had run green before the override). The first job of the next session is to make it compile and pass: see "Next action".

### Done
- [x] S1 (all), review fixes, envtest infrastructure, oracle
- [x] S2: keyring, secrets, registries, AgentLink + `go/agent`, api/dns, platform/doctor; domains (repo/domains.rs, routes/domains.rs), doctor delegation/proxy/agent checks
- [x] S3: store builds/scans, GitHub App; build pipeline (job, steps, observe, evidence, rescan, worker, 16 scenarios), oci Verifier, tests/oci.rs, serve builds; routes builds, scans, vulnerabilities, source, Git-sourced apps, routes/git.rs (installations + webhook)
- [x] S4: controls, installs, backups, CI trust, SSO; notify (repo/notify.rs, notify.rs, notifier in serve), incidents + webhooks; export and detach
- [x] S5: usage, image policies, platform/evidence; routes/apps/evidence.rs + doctor graph/findings; previews (store, lifecycle, janitor, routes; webhook → Previews.OnPull); client.rs, host.rs; state.rs, lib.rs, routes/mod.rs
- [x] tests/http.rs: 44 of 44; tests/ops_store_pg.rs; agent tests/runtime.rs runs in the new `go-kind` CI job
- [x] G5: the whole `kuben` CLI (cli.Root with every command, env fallbacks): serve, migrate, doctor, reset-admin, setup-token, agent-token, setup, status, login, apps, deploy, logs, rollback, uninstall, upgrade-check, backup, restore, support-bundle, dns01-issuer, version (+ --bundle), copy-self; bootstrap.rs, bundle.rs, telemetry.rs (Prometheus with Rust's six metric names, RUST_LOG); serve.rs complete (upgrade.Migrate with backup first, first admin, retention budgets, backup watch + incident, install journal, activator warning, metrics exporter)
- [x] G6 (code side): `.goreleaser.yaml` (5 hub targets + Linux agent, Rust triple archive names, checksums.txt + cosign bundle as install.sh expects, SBOMs), `.github/workflows/release.yml` for v2.* tags (Go tests on PG + envtest, goreleaser, provenance, image, chart, install check, smoke); size budgets warn only
- [x] G7: crates/ and all Rust tooling removed; CI Go-only (e2e, e2e-build, host-install, k3s-install, budgets build the Go binaries); security.yml uses govulncheck; Dockerfile is a Go build; scripts and docs updated; ADR-033 (Go runtime) on the site; openapi.json and the CRD manifest are frozen files now

### Next action (in order)
1. **Make it compile and pass**, module by module, from go/hub, go/agent, go/kubenapi: `go build ./... && go vet ./... && test -z "$(go tool gofumpt -l .)"`, `go test -race ./...` with PostgreSQL and envtest, `bash ../../scripts/go-check.sh`, golangci-lint v2.13.2 with 0 issues (a local binary is at `.cache/go/golangci-lint`). Expect compile errors in the uncompiled units: cli/* (setup, client, backup, support, doctor, dns01, upgrade), bootstrap, platform/metrics wiring (health, supervise, controller, leader, projection source), api (previews, git, apps_evidence, apps_export, source, scans, vulnerabilities), serve (maintenance, background). Restore go.work.sum before committing.
2. Then open the PR (CI's `go`, `go-kind`, `oracle` jobs only run on pull requests) and fix what CI finds (database tests, envtest, kind, e2e jobs on the Go binaries).
3. Tidy: fold `serve.AdvertiseIP` and `api.WriteOwnerOnly` into `platform/host`; check the dns01 `--dry-run` YAML golden once against the v1.2.0 binary; `docs` configuration reference still documents Tokio `[runtime]` settings that 2.x ignores.
4. Release train (G6, operational): `v2.0.0-alpha.1` once CI and the oracle are green; beta/rc gates and cutover rehearsal per plan §14.
5. Local cleanup (owner): `target/` and `.cargo-home/` at the repository root are leftover Rust build output, no longer ignored by .gitignore — delete them to free disk.

### Owner decisions pending
- **Rust 1.2.0 bug (hotfix candidate):** `crates/kuben-platform/src/build/job.rs` SCAN_SCRIPT has a raw byte 0x01 where `\1` was meant in the sed that reads the Trivy DB date; whenever `trivy version` prints `UpdatedAt`, the report JSON contains a control byte, serde rejects it and every scan is recorded `unavailable` (the scan gate never sees findings). The Go port keeps the same bytes (pinned by TestScriptsAreRustsBytes). Fix both (literal `\1`) together.
- ghinstallation not used (hand-written token cache kept?); hub depends on go/agent (or move the hub link into kubenapi); automemlimit for the agent; regenerate AgentLink byte fixtures with the (now removed) Rust generator from 86ce940; contract int widths for CI trust (int64/int32 vs u64/u32).
