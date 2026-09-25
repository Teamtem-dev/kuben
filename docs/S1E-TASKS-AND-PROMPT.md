# Kuben Go rewrite — فهرست کارهای باقی‌مانده و پرامپت اجرا

تاریخ: ۲۰۲۶‑۰۹‑۲۳ · شاخهٔ محلی: `feat/go-rewrite` · head: `c9421b5` · درخت کاری تمیز
مرجع‌ها: `docs/HANDOFF-GO.md`، `docs/KUBEN-GO-REWRITE-PLAN.md` (v2.1)، `go/CONVENTIONS.md`، `go/PARITY.md`، `go/SUBSTITUTIONS.md`

---

## بخش الف — وضعیت و فهرست کارها (فارسی)

### انجام‌شده
- G0 پایه، G1 هستهٔ `kuben-core`، G2 اسکلت راه‌رونده (setup، login، projects list/get، SSE، کنسول).
- S1-A…D: kubenapi (CRDها)، store، render، projection، discovery، leader، registry، supervise، materializer، controllerها، وصل‌شدن همه به `serve`.
- S1-E تا اینجا: پروژه (create/delete)، environment (list/get/create/delete)، app (list/create/get/update/delete/restart/handover)، deployment (start با Idempotency-Key، list، get)، release (list، rollback)، `api/oci` (tag → digest روی go-containerregistry)، اعتبارسنج‌های مشترک، admission.
- تست‌ها: تمام تست‌های واحد Rust این فایل‌ها پورت شده؛ سناریوهای `tests/http.rs` مربوطه پورت شده و در CI (PostgreSQL) اجرا می‌شوند.

### باقی‌ماندهٔ S1-E (به ترتیب)
| # | کار | فایل Rust مبدأ | تست Rust | معیار پایان |
|---|---|---|---|---|
| 1 | promote (ارتقا به environment دیگر، dry-run با diff) | `routes/apps/promote.rs` | 1 | route + تست واحد + `scenario10` از `tests/http.rs` اگر وجود دارد |
| 2 | jobs (اجرای دستی job زمان‌بندی‌شده) | `routes/apps/jobs.rs` | 1 | route + تست |
| 3 | logs و events (یک‌بار و follow) | `routes/apps/logs.rs` | 3 | route + ۳ تست؛ بدون کلاستر باید `503` بدهد (همان assertion در `environments_and_apps_read_from_sql`) |
| 4 | domains یک app (بررسی DNS) | `routes/apps/domains.rs` | 0 | route؛ resolver قابل تزریق برای تست |
| 5 | approvals (فهرست، decide) | `routes/apps/approvals.rs` | 2 | route + ۲ تست + `m4_protected_deploys_wait_for_another_approver` |
| 6 | policy (خواندن/تنظیم policy محیط) | `routes/policy.rs` | 2 | route + ۲ تست + `m4_weakening_protection_takes_an_owner` |
| 7 | templates (کاتالوگ و deployTemplate) | `routes/templates.rs` | 2 | route + ۲ تست + `scenario8_template_catalogue` |
| 8 | status (نمای کلی وضعیت) | `routes/status.rs` | 0 | route + تست |
| 9 | doctor یک app | `routes/apps/doctor.rs` | 0 | فقط اگر به کلاستر/Gateway وابسته نیست؛ وگرنه S2 |
| 10 | سناریوهای oracle | `go/hub/test/oracle/` | — | افزودن سناریوی پروژه/environment/app/deployment/release/rollback به oracle؛ همان بایت‌ها با Rust 1.2.0 |
| 11 | بستن S1 | `go/PARITY.md` | — | هیچ ردیف `todo` برای فایل‌های S1؛ همهٔ `m1_*` و scenario1…10 پورت |

### وابستگی‌هایی که عمداً به برش‌های بعد سپرده شده (نباید در S1 وارد شود)
- ساخت app از Git (`source.rs`, `builds.rs`) → S3. فعلاً `501`.
- registry login برای resolve و Secretهای مدیریت‌شده در materializer → keyring در S2. فعلاً anonymous / `SecretsUnavailable`.
- `RegistryVerifier` در `oci.rs` → S3.
- export/detach route (`export.rs`) → S4؛ store آن پورت شده.
- doctor کلاستر، دامنه‌ها/Gateway/cert، agent → S2.

### قواعد الزامی (خلاصه؛ کامل در `go/CONVENTIONS.md`)
- هرگز `git push`، PR، tag. فقط کامیت محلی با Conventional Commits و خط `Co-Authored-By`.
- قراردادها فریز: JSON، کدهای خطا، رشته‌های audit، SQL، CRD. برای هر فیلد nullable که Rust بدون `skip_serializing_if` می‌نوشت، در ogen باید صریحاً `SetToNull` شود.
- `opt.Val[T]` به‌جای `*T`؛ بازگشت `(T, bool)`؛ NilAway بدون `nolint`.
- هر فایل Rust که پورت می‌شود، همهٔ تست‌هایش پورت می‌شود و ردیف PARITY به‌روز می‌شود؛ هر جایگزینی کتابخانه یک ردیف در SUBSTITUTIONS.
- `go mod tidy` فقط عمداً و وقتی agent دیگری روی ماژول کار نمی‌کند.
- پیش از هر دستور Go: `. .cache/go/env.sh`. ماژول‌ها از mirror محلی؛ کمبود با `python3 .cache/go/gofetch.py loop -- <cmd>`.
- بدون Playwright/مرورگر سنگین. PostgreSQL در sandbox نیست: تست‌های DB محلی skip می‌شوند و CI با `KUBEN_REQUIRE_PG=1` اجرا می‌کند؛ پس منطق تست‌های DB را با خواندن SQL تأیید کن.

### دستورهای تأیید
```bash
. /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/env.sh
cd /Users/fa/Desktop/kubex/kuben-monorepo/go/hub && go build ./... && bash ../../scripts/go-check.sh && go test -race -shuffle=on ./...
cd ../kubenapi && go test -race ./... && bash ../../scripts/go-check.sh
```
(لینک نهایی `cmd/kuben` در sandbox به‌خاطر نبود `dsymutil` شکست می‌خورد؛ این خطا را نادیده بگیر.)

---

## بخش ب — پرامپت اجرا (انگلیسی، برای Gemini یا هر agent دیگر)

(متن بین دو خط `=====` را کامل کپی کن.)

=====

You are continuing the Go rewrite of **Kuben**, a self-hosted Kubernetes PaaS, in the monorepo `/Users/fa/Desktop/kubex/kuben-monorepo`, local branch `feat/go-rewrite` (head `c9421b5`, clean tree). The Rust product (crates/*, version 1.2.0) is frozen and is the reference; the Go rewrite must reproduce its behaviour and wire contracts exactly, fix genuine Rust weaknesses only when clearly safe, and drop code that existed only because of Rust.

## 0. Binding rules from the owner
1. **Never `git push`, never touch a remote (no PRs, no tags).** Commit locally only, one logical unit per commit, Conventional Commits (`feat(go): …`, `fix(go): …`, `docs(go): …`), body says *why*, last line exactly: `Co-Authored-By: <your model name> <noreply@anthropic.com>` replaced by the attribution your harness prescribes.
2. Replies to the owner in **Persian**; code, comments, logs, commit messages, ledger rows in **English**.
3. Before any Go command: `. /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/env.sh`. Modules come from a local mirror; if a command reports a missing module, run `python3 /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/gofetch.py loop -- <the go command>` from `go/hub` (network only to proxy.golang.org and storage.googleapis.com). Run `go mod tidy` only deliberately, only when no other agent works on the module, and never as a side effect.
4. No Playwright, no heavy local browser. PostgreSQL cannot run in this sandbox: database tests skip locally and run in CI (`KUBEN_TEST_PG_URL`, `KUBEN_REQUIRE_PG=1`), so verify every database-backed test by reading the SQL and the store code it depends on. Kubernetes tests use client-go fakes or controller-runtime's fake client; envtest binaries are not available locally.
5. Quality over binary size. Standard library first; a new dependency only when plan §5 names it, and then with a row in `go/SUBSTITUTIONS.md`.
6. `/Users/fa/Desktop/kubex/kuben-monorepo/docs/` is local and gitignored; keep `docs/HANDOFF-GO.md` current at the end of your session.

## 1. Read first, in this order (do not skip)
1. `docs/HANDOFF-GO.md` — state, sandbox gotchas, pending work, §8 = immediate next action.
2. `go/CONVENTIONS.md` — mandatory Go rules. Key ones: `opt.Val[T]` instead of `*T` for "maybe"; functions that may not find something return `(T, bool)` or `(T, error)`, never a nil pointer with a nil error; NilAway findings are build failures and are fixed with guards, never `nolint`; Rust data enums become sealed interfaces with `//sumtype:decl`; string enums are named string types with `ParseX`; every `switch` over them is exhaustive; no `init()`, no package-level mutable state, every goroutine owned and cancellable; tests table-driven in the external `x_test` package with go-cmp and the standard library; gofumpt; doc comment on every exported identifier; functions ≤ 100 lines.
3. `docs/KUBEN-GO-REWRITE-PLAN.md` (v2.1, Persian) — especially §6 (frozen contracts), §14 (release train), §19–§22.
4. `go/PARITY.md` (Rust file → Go status; a file is `ported` only when every one of its tests has a Go counterpart) and `go/SUBSTITUTIONS.md` (every library swap and known difference).
5. `go/hub/internal/api/apps*.go`, `environments.go`, `projects.go`, `scope.go`, `request.go`, `validate.go` — the route style you must match: `s.access(ctx)` → `findProject/findEnvironment/findApp` → `a.Require(perm, chain)` → one `Tenant` transaction with `defer t.Rollback` and an explicit `Commit` → DTO. Errors are `kerr.New(kerr.<Code>, …)`; store errors are returned as they are (`//nolint:wrapcheck // a store error, answered as internal`).

Then run `git status` and `git log -5` and confirm they match the handoff.

## 2. Frozen contracts you must reproduce
- **JSON**: field names, enum strings, error codes (`kerr` codes → problem+json), audit action strings, `Location` headers, status codes. Read the serde attributes of each Rust DTO (`rename_all`, `rename`, `skip_serializing_if`, `default`, `tag`, `untagged`, `flatten`). In ogen, an **unset `OptNil*` member is omitted, but Rust wrote `null` for every `Option` without `skip_serializing_if`**: set such members explicitly (`optNilString`, `optNilBool`, `SetToNull()`); leave unset only what Rust skipped.
- The API server is generated by **ogen** from the frozen `packages/api-client/openapi.json`; implement handlers on `*Server` (`go/hub/internal/api`); the generated interface is in `internal/api/gen/oas_server_gen.go`. Operations not ported answer 501 through `UnimplementedHandler`. Response headers that ogen cannot express go through `httpx.SetHeader(ctx, key, value)`.
- **SQL** is copied verbatim from the Rust store; the Go store already has most reads/writes (`go/hub/internal/store`). Add missing ones next to their Rust counterparts, with the same SQL text, and port their tests.
- Integers: Rust `u16/u32/u64` fields arrive as ogen `int32/int64`; range-check what the contract's schema does not (e.g. `port ≤ 65535`) and refuse as serde would (422).
- Hashes and stored JSON go through `wire.CanonicalValue` / `wire.DecodeAny` (numbers kept as `json.Number`); never `json.Unmarshal` into `any`.

## 3. Tasks, in order (each one: port → tests → checks → ledgers → commit → short Persian report)
Port these Rust files from `crates/kuben-api/src/routes/` into `go/hub/internal/api/`, one file group per commit:

1. **`apps/promote.rs`** (358 lines, 1 test) → `apps_promote.go`: promotion of an app to another environment of the same project with dry-run diff; reuse `deployChangeFor`, `specOf`, `desiredSpec`, `validateSpec`, `ensureDomainsFree`. Missing-secret warnings need the cluster's Secrets (list through `s.cluster()` and the typed client; without a cluster the warning list is empty as in Rust—check the Rust behaviour first). Port the unit test and, if present in `crates/kuben-api/tests/http.rs`, the promotion scenario (`scenario10…`).
2. **`apps/jobs.rs`** (105 lines, 1 test) → `apps_jobs.go`: "run now" of a scheduled process (creates a Job from the CronJob through the cluster; `kubeError` mapping; 503 without a cluster if that is what Rust did).
3. **`apps/logs.rs`** (616 lines, 3 tests) → `apps_logs.go`: log lines once or followed (SSE/stream), and Kubernetes events; `environments_and_apps_read_from_sql` expects **503** for logs without a cluster—add that assertion to `TestAppsReadFromSQL`. Use client-go's pod log streams; keep Rust's limits and framing.
4. **`apps/domains.rs`** (90 lines) → `apps_domains.go`: DNS checks of an app's hostnames; make the resolver injectable (an interface on `Deps`) so tests need no network.
5. **`apps/approvals.rs`** (309 lines, 2 tests; `ensure_may_deploy` is already in `apps.go`) → `apps_approvals.go`: list approvals of a run and decide (approve/reject with plan hash and comment); store functions from `repo/policies.rs` (`decide`, `approvals_of_run`, …) with their SQL; port the 2 unit tests and `m4_protected_deploys_wait_for_another_approver` from `tests/http.rs`.
6. **`policy.rs`** (299 lines, 2 tests) → `policy_routes.go`: get/set an environment's policy (`SetEnvironmentPolicy` exists in the store); port the tests and `m4_weakening_protection_takes_an_owner`.
7. **`templates.rs`** (543 lines, 2 tests) → `templates.go`: the template catalogue and `deployTemplate` (which calls `createApp`); port the tests and `scenario8_template_catalogue`.
8. **`status.rs`** (322 lines) → `status.go`: the status overview built from SQL and the projections.
9. **`apps/doctor.rs`** (231 lines): only if it does not depend on the Gateway/cert work of slice S2; otherwise leave it and say so.
10. **Oracle scenarios**: extend `go/hub/test/oracle/` (see `oracle.go`, `oracle_test.go`) with a scenario covering project create → environment create → app create (image by digest) → deployment start/poll → PATCH → releases → rollback → deletes, so CI compares the Go server with the released Rust 1.2.0 byte for byte after normalization.
11. **Close S1**: every `crates/kuben-api/src/routes/**` file S1 needs has no `todo` row in `go/PARITY.md`; every `scenario1…scenario10`, `a_deployment_*`, `viewers_*`, `environments_and_apps_*`, `projects_*` test of `tests/http.rs` has a Go counterpart; update `docs/HANDOFF-GO.md` §4–§8.

Do **not** pull later slices into S1: Git-sourced apps and builds (S3, answer 501), registry logins / secret keyring / managed Secrets (S2, resolve anonymously and fail `SecretsUnavailable` as Rust without a keyring), `RegistryVerifier` (S3), export/detach routes (S4), doctor/domains/Gateway/cert/agent (S2).

## 4. Definition of done for every task
- `go build ./...`, `go vet ./...` clean (from `go/hub`; the final link of `cmd/kuben` fails in this sandbox for lack of `dsymutil`—ignore only that).
- `bash ../../scripts/go-check.sh` clean: gofumpt, vet, exhaustive, go-check-sumtype, NilAway (fix findings with guards or `(T, bool)` returns, never `nolint`).
- `go test -race -shuffle=on ./...` green; database tests skip locally and are verified by reading the SQL.
- Every Rust test of the ported file has a Go counterpart (name it after the Rust test, `TestSnakeCaseInCamelCase`), and the mapping (`N → M Go tests`) is in the PARITY row.
- Each deliberate behaviour difference is recorded in `go/SUBSTITUTIONS.md` with why it is acceptable, or in the PARITY note.
- One local commit per task, message explains why, attribution line present. Never commit generated or stale files, `docs/` or `.cache/`.
- After each task: a short Persian report to the owner (what was ported, test mapping, differences, what is left), then continue without stopping. Stop and ask only for a decision that is genuinely the owner's (a contract change, a new dependency not named in the plan, removing a feature).

## 5. Verification commands
```bash
. /Users/fa/Desktop/kubex/kuben-monorepo/.cache/go/env.sh
cd /Users/fa/Desktop/kubex/kuben-monorepo/go/hub && go build ./... && bash ../../scripts/go-check.sh && go test -race -shuffle=on ./...
cd ../kubenapi && go test -race ./... && bash ../../scripts/go-check.sh
cd ../.. && scripts/rust-drift.sh
```

## 6. Report format at the end of the session (Persian, concise)
1. Commits made (hash + one line each).
2. PARITY/SUBSTITUTIONS rows added or changed.
3. Every deliberate difference from Rust and its reason.
4. What is left of S1-E and the exact next action, written into `docs/HANDOFF-GO.md` §8.

=====
