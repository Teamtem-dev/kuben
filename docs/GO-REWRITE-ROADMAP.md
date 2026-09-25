# Kuben Go rewrite — نقشهٔ راه کامل تا حذف Rust

تاریخ: ۲۰۲۶‑۰۹‑۲۳ · شاخه: `feat/go-rewrite` · head: `c9421b5` · plan مرجع: `docs/KUBEN-GO-REWRITE-PLAN.md` v2.1
ledgerها: `go/PARITY.md` (۲۳۱ فایل Rust: ۱۱۴ ported، ۱۵ partial، ۲ dropped، **۱۰۰ todo**)، `go/SUBSTITUTIONS.md`

> **پاسخ کوتاه:** نه. با پایان S1-E فقط **برش S1** تمام می‌شود، یعنی حدود نیمی از فایل‌های Rust. تا رسیدن به «کل پروژه با Go و بدون Rust» این مراحل مانده‌اند: S1-E (باقی‌مانده) → S2 → S3 → S4 → S5 → G5 → F1…F3 (کنسول، موازی) → G6 (قطار انتشار و cutover) → G7 (حذف Rust). این سند همهٔ آن‌ها را به task تبدیل می‌کند.

---

## ۰. شمای کلی

| مرحله | محتوا | ورودی Rust (فایل‌ها / تست‌ها) | وضعیت |
|---|---|---|---|
| G0–G2 | پایه، core، اسکلت راه‌رونده | — | ✅ |
| S1-A…D | CRD، store، render/projection/discovery، materializer، controllerها، serve | — | ✅ |
| S1-E | routeهای API هستهٔ M1 | ۹ فایل route + oracle | ⬜ ۵۰٪ |
| S2 | image و HTTPS: OCI، domains/Gateway/cert، doctor، agent+AgentLink، registry login، secrets/keyring | ~۲۲ فایل، ~۸۵ تست | ⬜ |
| S3 | Git → build: GitHub App، BuildRun، BuildKit، scan/evidence | ~۲۰ فایل، ~۹۰ تست | ⬜ |
| S4 | MVP عملیاتی: policy/approval، controls، incidents/webhooks، backup، upgrade journal، detach، SSO، CI trust | ~۲۰ فایل، ~۵۰ تست | ⬜ |
| S5 | previews، domain claims/DNS، status، image policy، usage/rollups، evidence graph | ~۱۵ فایل، ~۱۵ تست | ⬜ |
| G5 | CLI (setup, doctor, backup, support, upgrade, dns01, agent, admin)، bootstrap، bundle | ~۲۰ فایل، ~۵۰ تست | ⬜ |
| F1–F3 | کنسول جدید (shadcn) روی هر دو باینری | apps/console | F0 ✅، بقیه ⬜ |
| G6 | قطار انتشار alpha→beta→rc→2.0.0 و cutover C0…C3 | — | ⬜ |
| G7 | حذف `crates/` و همهٔ زیرساخت Rust | — | ⬜ |

قاعدهٔ ثابت هر برش (plan §۸): store + platform + routeها + سناریوهای `tests/http.rs` همان milestone؛ خروج = تست‌های Rust همه پورت و سبز (PG واقعی، envtest در CI)، oracle سبز، ردیف PARITY «ported/reviewed»، بدون یافتهٔ lint.

---

## ۱. S1-E — باقی‌مانده (جزئیات در `docs/S1E-TASKS-AND-PROMPT.md`)

| # | task | Rust | تست |
|---|---|---|---|
| 1.1 | promote | `routes/apps/promote.rs` | 1 (+ سناریوی promotion در http.rs) |
| 1.2 | jobs (run now) | `routes/apps/jobs.rs` | 1 |
| 1.3 | logs/events (once + follow) | `routes/apps/logs.rs` | 3 (+ assertion 503 بدون کلاستر) |
| 1.4 | app domains (DNS check، resolver تزریقی) | `routes/apps/domains.rs` | 0 |
| 1.5 | approvals list/decide | `routes/apps/approvals.rs`، `repo/policies.rs` (decide) | 2 + `m4_protected_deploys_wait_for_another_approver` |
| 1.6 | policy get/set | `routes/policy.rs` | 2 + `m4_weakening_protection_takes_an_owner` |
| 1.7 | templates + deployTemplate | `routes/templates.rs` | 2 + `scenario8_template_catalogue` |
| 1.8 | status overview | `routes/status.rs`، `repo/status.rs` | 1 |
| 1.9 | oracle: سناریوی project→env→app→deploy→PATCH→releases→rollback→delete | `go/hub/test/oracle` | — |
| 1.10 | بستن S1: PARITY بدون todo برای فایل‌های S1؛ `tests/http.rs` بخش‌های M1 پورت؛ HANDOFF به‌روز | — | — |

**خروج S1:** `2.0.0-alpha.1` قابل تگ (بدون push): CI سبز، oracle روی مسیرهای S1 سبز، یادداشت «هنوز نیست».

---

## ۲. S2 — image و HTTPS (M2)

| # | task | Rust | تست Rust |
|---|---|---|---|
| 2.1 | secret keyring (seal/open، reseal، rotation) | `kuben-platform/src/secrets.rs` | 10 |
| 2.2 | store secrets: revisions، bindings، registry logins، stale seals | `repo/secrets.rs` (باقی‌مانده) | 5 |
| 2.3 | routes secrets + registries | `routes/secrets.rs`، `routes/registries.rs` | 2 + 2؛ `m4_secret_values_are_revisions_rolled_out_to_their_apps`، `m4_production_rotations_wait_for_approval`، `m4_registry_logins_pull_private_images` |
| 2.4 | materializer secrets (write/collect revision Secrets) و resolve با login | `materializer/secrets.rs`، `routes/apps/mod.rs::resolve` | 3 |
| 2.5 | serve: `secret_keyring()` در startup، `keep_budgets`، `watch_backups` (بخش secrets) | `serve.rs` | — |
| 2.6 | domains: verified domains، DNS providers، claims، dns01 (store + routes + `dns.rs`) | `repo/domains.rs`، `routes/domains.rs`، `dns.rs` | 2 + 4 (کتابخانهٔ DNS: `miekg/dns` یا stdlib طبق plan §۵) |
| 2.7 | Gateway/cert: بقیهٔ gateway controller (اگر چیزی مانده)، cert-manager Certificate در render | `controller/gateway.rs` (بازبینی) | — |
| 2.8 | doctor (platform + routes + app doctor) | `kuben-platform/src/doctor.rs`، `routes/apps/doctor.rs` | 4 |
| 2.9 | agent protocol + AgentLink (mTLS frame protocol، handshake، ماتریس نسخهٔ N/N‑1) | `agentlink.rs`، `kuben-agent/src/{protocol,link,tls,hub,state,runtime,enroll,bootstrap,main}.rs`، `kubenapi/protocol` | 3 + 9+3+6+4+4+9+2+3؛ `tests/agent_link_mtls.rs` (6)، `kuben-agent/tests/{link,runtime}.rs` (15+3) |
| 2.10 | store agents: enrollment، links، tokens، revoke | `repo/agents.rs` (باقی‌مانده) | 7 |
| 2.11 | local_agent (sync CA، enrollment داخل کلاستر) | `local_agent.rs` | 2 |
| 2.12 | baseline `go/agent`: باینری agent با discovery استاندارد، بودجهٔ RSS | `kuben-agent` | — |
| 2.13 | `tests/execution_crds.rs`، `tests/two_writer_cas.rs` (envtest، CI) | — | 2 + 3 |
| 2.14 | oracle: سناریوهای secrets/registries/domains | — | — |

**خروج S2:** hub 2.0 ↔ agent 1.2 و 2.0 هر دو کار می‌کنند (ماتریس mixed در CI)؛ HTTPS با cert-manager روی kind سبز؛ `2.0.0-alpha.2`.

---

## ۳. S3 — Git → build ایزوله (M3)

| # | task | Rust | تست |
|---|---|---|---|
| 3.1 | GitHub App client (installations، heads، webhooks) | `github.rs`، `routes/git.rs`، `routes/apps/source.rs` | 6 + 0 + 3 |
| 3.2 | store builds | `repo/builds.rs` | 11 |
| 3.3 | build worker: job، steps، observe، scenarios، evidence، rescan | `build/{mod,job,steps,observe,scenarios,evidence,rescan,worker}.rs` | 0+9+9+4+17+2+1+4 |
| 3.4 | RegistryVerifier (بخش باقی‌ماندهٔ `oci.rs`) و `image_watch.rs` | `oci.rs`، `image_watch.rs` | 2 + 0 |
| 3.5 | routes builds، scans، vulnerabilities، evidence | `routes/apps/{builds,scans,evidence}.rs`، `routes/vulnerabilities.rs` | 2+0+2+1؛ `m4_the_scan_gate_refuses_known_critical_findings` |
| 3.6 | store scans (باقی‌مانده) | `repo/scans.rs` | 3 |
| 3.7 | serve: `spawn_builds`، `ensure_build_namespace`، rescans | `serve.rs` | — |
| 3.8 | ساخت app از Git (رفع 501 موقت در `CreateApp`) | `routes/apps/source.rs::create_git_app` | — |
| 3.9 | oracle: سناریوی git/build (با GitHub App جعلی) | — | — |

**خروج S3:** بیلد end-to-end روی kind با BuildKit؛ `2.0.0-alpha.3`.

---

## ۴. S4 — MVP عملیاتی (M4)

| # | task | Rust | تست |
|---|---|---|---|
| 4.1 | controls: freeze، pause، emergency، silences، owners (store + routes) | `repo/controls.rs`، `routes/controls.rs` | 2 + 2؛ `m4_freezes_pauses_and_emergency_rollbacks`، `m4_owners_and_silences_are_kept` |
| 4.2 | incidents + notify (webhooks، deliveries، retention) | `notify.rs`، `repo/notify.rs`، `routes/incidents.rs` | 6 + 3 + 2 |
| 4.3 | backups (store + serve `watch_backups`) | `repo/backups.rs` | 2 |
| 4.4 | upgrade journal، installs | `repo/installs.rs`، `serve.rs::record_install_journal` | 1 |
| 4.5 | export/detach routes | `routes/apps/export.rs` | 2 |
| 4.6 | SSO (OIDC client، routes، store) | `sso.rs`، `auth/sso.rs`، `repo/sso.rs`، `oidc.rs` | 5+1+3+5؛ `m4_sso_*` (3) |
| 4.7 | CI trust (GitHub Actions OIDC exchange) | `routes/ci.rs`، `repo/ci.rs` | 3 + 3؛ `m4_untrusted_ci_tokens_get_nothing`، `m4_trusted_ci_gets_a_scoped_token_once` |
| 4.8 | project roles / quotas باقی‌مانده | `m4_project_roles_are_granted_without_escalation` (✅)، `m4_quotas_bound_what_apps_may_request` | — |
| 4.9 | serve: `spawn_background` (retention budgets، notify، …) | `serve.rs` | — |
| 4.10 | oracle: سناریوهای controls/incidents/sso | — | — |

**خروج S4:** همهٔ `m4_*` سبز؛ `2.0.0-alpha.4`.

---

## ۵. S5 — پیش‌نمایش و UX (M5)

| # | task | Rust | تست |
|---|---|---|---|
| 5.1 | previews (store، platform، routes، `previews.rs` API) | `repo/previews.rs`، `previews.rs`، `routes/previews.rs` | 2 + 0 + 0 |
| 5.2 | domain claims و DNS providers (بخش UX) | `routes/domains.rs` (باقی‌مانده) | — |
| 5.3 | status page | `routes/status.rs`، `repo/status.rs` (اگر در S1 نشد) | 1 |
| 5.4 | image policies | `repo/image_policies.rs`، `routes/apps/image_policy.rs` | 1 + 0 |
| 5.5 | usage و rollups (`UsageBuffer`، `spawn_usage`) | `kuben-platform/src/usage.rs`، `repo/rollups.rs`، `routes/apps/metrics.rs` | 4 + 1 + 0 |
| 5.6 | evidence graph | `kuben-platform/src/evidence.rs` | 5 |
| 5.7 | `host.rs`، `client.rs` (کلاینت API برای CLI) | `host.rs`، `client.rs` | 2 + 4 |
| 5.8 | oracle: سناریوهای previews/status | — | — |

**خروج S5:** feature-complete backend؛ `2.0.0-beta.1` پس از G5.

---

## ۶. G5 — CLI، bootstrap، bundle، upgrade، backup

| # | task | Rust | تست |
|---|---|---|---|
| 6.1 | `cli/mod.rs`، `cli/ui.rs` (spinner، prompts)، `main.rs` باقی‌مانده | — | 2 + 3 |
| 6.2 | `kuben setup` (plan، platform، journal) | `cli/setup/{mod,plan,platform,journal}.rs` | 10+1+6+4 |
| 6.3 | `bootstrap.rs` (admin seed، hand-over password) | `bootstrap.rs` | 3 |
| 6.4 | `bundle.rs`، `cli/support.rs` (support bundle) | — | 3 + 4 |
| 6.5 | `cli/backup.rs`، `cli/upgrade.rs`، `cli/doctor.rs`، `cli/dns01.rs`، `cli/agent.rs`، `cli/admin.rs` | — | 5+1+2+2+0+0 |
| 6.6 | `telemetry.rs` (metrics/OTLP؛ شمارنده‌های `kuben_reconcile_errors_total` و …) | `telemetry.rs` | 0 |
| 6.7 | `serve.rs` کامل (activator: نادیده با هشدار؛ `keep_budgets`؛ …)، `state.rs`، `lib.rs` | — | — |
| 6.8 | golden خروجی CLI برابر با Rust؛ `scripts/e2e.sh` روی باینری Go | — | — |
| 6.9 | ارتقای درجا از نصب Rust 1.2.x (DB + agent) و برگشت | — | — |

**خروج G5:** `scripts/e2e.sh` سبز؛ golden CLI برابر؛ upgrade/rollback سبز.

---

## ۷. مسیر F — کنسول جدید (موازی، شاخهٔ `feat/console-shadcn`)

| فاز | task | خروج |
|---|---|---|
| F0 ✅ | shadcn، theme.css، پوسته، RTL codemod، CSP | انجام شده (۵ کامیت) |
| F1 | login، setup، account، projects، project، environment، app (تب‌ها)، deployments، logs | هم‌ارزی کارکردی با کنسول فعلی |
| F2 | incidents، webhooks، domains، previews، status، detached، image policy، usage، doctor، team، tokens، audit؛ حذف `components/ui.tsx` | هم‌ارزی کامل |
| F3 | داشبورد خانه، DataTable، skeleton/empty، Playwright + axe، snapshot بصری | بودجهٔ باندل (JS اولیه ≤ ۲۰۰ kB brotli)، a11y، CSP سبز |
| F-rel | انتشار کنسول به‌صورت **1.3.0 روی backend Rust** | — |

---

## ۸. G6 — قطار انتشار و cutover (plan §۱۴)

| # | task |
|---|---|
| 8.1 | `2.0.0-alpha.N` بعد از هر برش: CI + oracle سبز، یادداشت «هنوز نیست» |
| 8.2 | `2.0.0-beta.N`: همهٔ سناریوهای پذیرش؛ ارتقای درجا از 1.2.x و برگشت؛ ماتریس agent/hub؛ gateهای §۱۱ (RSS، p95 ≤ ۱٫۲× Rust)، soak ۳۰ دقیقه؛ دو beta پیاپی بدون رگرسیون قرارداد |
| 8.3 | تمرین cutover C0 (سایهٔ read-only) → C1 (api) → C2 (controller) → C3 (agentها) با rollback مستند |
| 8.4 | `2.0.0-rc.N`: انجماد کد؛ مرور امنیتی (gosec، govulncheck، secretها، CSP)؛ مستندات و راهنمای ارتقا؛ ۷ روز بدون P0/P1 |
| 8.5 | `2.0.0` = همان commit آخرین rc؛ **هیچ مهاجرت DB جدید** (اولین مهاجرت `0035` در 2.1.0) |
| 8.6 | PGO (`default.pgo` از soak)، benchstat در CI، `GOMEMLIMIT` |

---

## ۹. G7 — حذف Rust (در ورود به rc)

| # | task |
|---|---|
| 9.1 | حذف `crates/`، `Cargo.*`، `rust-toolchain.toml`، `clippy.toml`، `.cargo/`، nextest، taskهای cargo در `turbo.json`، zig/cargo-auditable در release، cargo-audit در security |
| 9.2 | `scripts/rust-drift.sh` و `compat_fixtures` → fixtureها به‌صورت فایل‌های ثابت می‌مانند؛ oracle از **image منتشرشدهٔ 1.2.0** استفاده می‌کند |
| 9.3 | `kuben-crd`/`crdgen` → `go/kubenapi` + controller-gen؛ manifest CRD از Go تولید و با نسخهٔ فریز مقایسه شود |
| 9.4 | ADR‑0033 (Go runtime)، به‌روزرسانی ADR‑0024؛ مستندات سایت و `docs/`؛ `m0-spikes.yml`، `docs/archive`، snapshotهای schemars حذف |
| 9.5 | PARITY: هیچ ردیف todo/partial؛ ledger به «تاریخچه» تبدیل شود |
| 9.6 | شاخهٔ `release/1.2` برای رفع امنیتی تا ۶ ماه بعد از GA |

---

## ۱۰. تخمین حجم

| مرحله | خطوط Rust تقریبی | تست Rust |
|---|---|---|
| S1-E باقی‌مانده | ~۳٬۰۰۰ | ~۱۲ |
| S2 | ~۹٬۰۰۰ | ~۸۵ |
| S3 | ~۷٬۵۰۰ | ~۹۰ |
| S4 | ~۷٬۰۰۰ | ~۵۰ |
| S5 | ~۳٬۵۰۰ | ~۱۵ |
| G5 | ~۸٬۰۰۰ | ~۵۰ |
| **جمع** | **~۳۸٬۰۰۰** | **~۳۰۰** |

با نرخ فعلی (هر برش چند جلسهٔ agent)، backend feature-complete پس از S5+G5 و انتشار 2.0.0 پس از G6 قابل انتظار است؛ کنسول موازی.
