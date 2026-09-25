# Kuben — پلن بازنویسی با Go (نسخهٔ ۲٫۱)

تاریخ: ۲۰۲۶‑۰۹‑۲۱ · وضعیت: **منتظر تأیید نهایی** · جایگزین نسخه‌های ۱ و ۲
مبنای Rust (baseline): `main` در `c1f94cf` + شاخهٔ `feat/cli-spinner-and-http-setup` در `009d0d8` · نسخهٔ محصول 1.2.0

> **هدف:** Kuben با Go، پشت همان قراردادها (API، اسکیمای PostgreSQL، CRDها، پروتکل agent، config، CLI، کنسول، چارت)، با کیفیت مهندسی بالاتر از نسخهٔ Rust؛ در پایان حذف کامل Rust.
> **این سند چه چیزی نیست:** وعدهٔ «سریع‌تر اجرا شدن». سود این کار در سرعت توسعه، اکوسیستم Kubernetes و جذب نیرو است (§۱).

**نسخهٔ ۲٫۱ (درخواست‌های ۰۹‑۲۱):** قطار انتشار alpha → beta → rc → 2.0.0 (§۱۴)، هرس کد و فیچر اضافی (§۱۹)، کد تمیز و hookها (§۲۰)، مهندسی کارایی (§۲۱)، بازسازی کامل کنسول با shadcn/ui (§۲۲).

**تغییرات نسخهٔ ۲ نسبت به ۱** در پیوست ب فهرست شده؛ مهم‌ترین‌ها: ترتیب فازها از «لایه‌ای» به «اسکلت راه‌رونده + برش‌های عمودی»، cutover نقش‌به‌نقش با rollback، دفتر جایگزینی کتابخانه‌ها، JSON کانونی برای هش‌ها، spike تولیدکنندهٔ OpenAPI 3.1، و رویهٔ همگام‌سازی با Rust که هنوز در حال تغییر است.

---

## ۱. هدف، غیرهدف و معیار موفقیت

| | |
|---|---|
| **چه چیزی بهتر می‌شود** | کتابخانه‌های رسمی Kubernetes/BuildKit/OCI/ACME به‌جای کد دست‌نویس؛ بیلد ثانیه‌ای و کراس‌کامپایل بدون ابزار جانبی؛ مخزن نیروی بسیار بزرگ‌تر؛ هم‌زبانی با k3s، Helm، cert-manager |
| **چه چیزی بهتر نمی‌شود** | سرعت اجرا (هم‌مرتبه)؛ مصرف حافظه (بدتر: agent حدود ۲۰ تا ۴۰ MiB به‌جای ۵ تا ۱۵)؛ تضمین‌های زمان کامپایل (با lint و تست جبران می‌شود، نه برابر) |
| **کاربر نهایی** | هیچ تغییری نباید ببیند: همان API، همان داده، همان کنسول، همان `helm upgrade` |
| **هزینه** | هم‌مرتبهٔ M1 تا M5 (حدود یک‌سوم کمتر)؛ در این مدت فیچر جدیدی به محصول اضافه نمی‌شود |

**معیار موفقیت (قابل اندازه‌گیری):** (۱) نصب 1.2.x با تعویض image و بدون دخالت دستی به Go ارتقا یابد و برگردد؛ (۲) همهٔ سناریوهای پذیرش پورت‌شده و تست تفاضلی با باینری Rust سبز؛ (۳) قرارداد API بدون تغییر، و کنسول جدید (§۲۲) روی **هر دو** باینری Rust و Go کار کند؛ (۴) gateهای §۱۱ سبز؛ (۵) هیچ فایل Rust در ریپو.

---

## ۲. آنچه از نسخهٔ Rust می‌ماند و آنچه درد داشت

### ۲.۱ طراحی‌هایی که عیناً حفظ می‌شوند

| تصمیم | مرجع |
|---|---|
| PostgreSQL مرجع محصول است؛ CRDها از SQL مادی می‌شوند | ADR‑0025، ADR‑0032 |
| Release → DeploymentRun → RenderPlan؛ ورودی‌های run تغییرناپذیر (trigger در SQL)؛ plan محتوانشانی‌شده | ADR‑0026 |
| مرز اجرای agent: agent همیشه dial می‌کند؛ hub فقط envelope می‌فرستد؛ agent فقط SSA می‌کند | ADR‑0027 |
| ایزوله‌سازی اعتماد بیلد: pod بیلد بدون credential کلاستر | ADR‑0028 |
| دسترسی رجیستری مستقل از کلاستر | ADR‑0029 |
| مرجع secret و revisionها؛ BYOK و دروازهٔ HTTPS | ADR‑0030، ADR‑0031 |
| رهبری کنترلر با Lease و CAS روی `resourceVersion`؛ انقضا بر اساس «مشاهدهٔ محلی بدون تغییر»، نه مقایسهٔ ساعت‌ها | ADR‑0023 |
| نقش‌ها در یک باینری (`api`، `controller`، `activator` رزرو) | serve.rs |

### ۲.۲ دردهای واقعی (با شاهد از ریپو)

| درد | شاهد | در Go |
|---|---|---|
| زمان بیلد | release با `embed-ui`: ۱۵ دقیقه و ۱۹ ثانیه | ثانیه‌ها |
| کراس‌کامپایل musl | وابسته به zig؛ بودجهٔ واقعی روی مک قابل سنجش نبود | `CGO_ENABLED=0 GOOS=linux` |
| کد Kubernetes دست‌نویس | `kuben-crd`+`crdgen`، `projection/informer.rs`، `leader.rs`، `crd_apply.rs` | controller-gen، controller-runtime |
| OCI/DNS/ACME/GitHub دست‌نویس | `oci.rs`، `dns.rs`، `cli/dns01.rs`، `transport.rs`، `github.rs` | کتابخانه‌های نگهداری‌شده (با قید §۳.۳) |
| OpenAPI کد-اول | utoipa؛ drift فقط با `git diff` | spec-اول |
| lint مکانیکی | `too_many_lines=100` و شکستن مصنوعی توابع | lintهای انتخابی و مستند |
| trait objectهای async، feature flagها، تست‌های PG که محلی بی‌صدا skip می‌شوند | `async-trait`، `embed-ui`/`activator`، `pg_store()` | interface ساده، بدون flag، testcontainers |

---

## ۳. اصول حاکم (هر تصمیم بعدی از این‌ها می‌آید)

1. **قراردادها قفل‌اند، کد آزاد است.** فهرست قراردادها در §۶ است و هر کدام fixture و تست دارد.
2. **parity اول.** بهبود فقط از فهرست بستهٔ §۱۳؛ هر چیز دیگر بعد از حذف Rust.
3. **جایگزینی کتابخانه = تغییر رفتار، تا خلافش ثابت شود.** (درس واقعی از G1: `Masterminds/semver` بازه‌ها را متفاوت از crate راست می‌خواند و `idna.Lookup` دامنه‌هایی را رد می‌کند که Rust می‌پذیرفت.) هر جایگزینی در **دفتر جایگزینی** (`go/SUBSTITUTIONS.md`) ثبت می‌شود: چه چیزی جایگزین شد، تفاوت‌های شناخته‌شده، تستی که رفتار Rust را پین می‌کند. بدون ردیف در دفتر، PR پذیرفته نمی‌شود.
4. **باینری Rust داور است.** تا cutover، تست تفاضلی (oracle) روی هر برش اجرا می‌شود.
5. **هر ضعف Go یک سازوکار ساختاری + یک gate دارد** (§۷)، نه توصیه.
6. **برش عمودی، نه لایهٔ افقی.** زودترین زمان ممکن یک باینری واقعی روی kind بالا می‌آید؛ غافلگیری‌های یکپارچه‌سازی را هفتهٔ اول می‌گیریم نه آخر.
7. **Rust در این مدت هدف متحرک است.** رویهٔ §۹.۴ مانع جاماندن می‌شود.
8. **هیچ push بدون دستور صریح.** همهٔ commitها محلی.

---

## ۴. معماری هدف

```
go.work                       use ( ./go/agent ./go/hub ./go/kubenapi ./go/tools )
go/
  CONVENTIONS.md  SUBSTITUTIONS.md  PARITY.md
  kubenapi/                   تایپ‌های CRD (v1alpha1، markerهای kubebuilder) + پروتکل hub↔agent
  agent/                      cmd/kuben-agent؛ فقط client-go/dynamic + SSA؛ بدون controller-runtime
  hub/                        باینری `kuben` (نام ماژول hub است چون Turborepo نام `kuben` را با crate فعلی یکی می‌بیند)
    cmd/kuben/
    internal/core/            دامنهٔ خالص، بدون IO (✅ پورت شده، §پیوست الف)
    internal/wire/            JSON کانونی، decoderهای سخت‌گیر، required[T]، بدون HTML-escape
    internal/store/           db، migrate (دفتر-سازگار)، tenant (RLS)، repo/*، queries/*.sql (sqlc)، pgq
    internal/platform/        controller، materializer، projection، render، build، doctor، evidence، usage، agentlink، registry، keyring، supervise
    internal/api/             gen/ (تولیدی از openapi.json)، routes/*، auth/*، oci، dns، github، notify، previews، imagewatch، stream، web
    internal/{serve,cli,bootstrap,bundle,telemetry}
  tools/                      ماژول جدا با `tool` directive: controller-gen، sqlc، تولیدکنندهٔ OpenAPI، nilaway، govulncheck، go-licenses (نسخه‌ها پین)
```

- **قاعدهٔ همتایی:** هر فایل Rust یک همتای Go با همان نام و یک ردیف در `PARITY.md` دارد (وضعیت، تعداد تست Rust/Go، مرورشده؟). بازآرایی بعد از parity.
- **جریان داده بدون تغییر:** `API → SQL → materializer → SSA (مستقیم یا از راه agent) → projection → SQL views → API/SSE`.
- **AgentLink:** همان سیم فعلی: استریم **mTLS خام** که agent باز می‌کند، فریم = طول `u32` big-endian + JSON تا `MAX_FRAME`، مذاکرهٔ نسخهٔ N و N‑1، پیام ناشناخته = `Unknown`. (نسخهٔ ۱ پلن اشتباهاً WebSocket را محتمل دانسته بود.)

---

## ۵. پشته (نهایی)

| لایه | انتخاب | نکتهٔ تعیین‌کننده |
|---|---|---|
| HTTP | `net/http` استاندارد | بدون فریم‌ورک؛ SSE با `http.Flusher` |
| OpenAPI | **ogen** (نتیجهٔ Spike ۱، `go/SPIKES.md`): فقط سرور، بدون OpenTelemetry؛ oapi-codegen روی همین spec کامپایل نمی‌شود | spec فعلی **OpenAPI 3.1.0** است (۱۲۹ عملیات، ۱۱۴ schema، ۱۸۱ بار `type:[T,"null"]`، ۱۰ `oneOf`). پشتیبانی 3.1 در تولیدکننده‌های Go ناقص است. معیار انتخاب: تولید بدون دست‌کاری spec، سرور strict، اعتبارسنجی درخواست. راه پشتیبان: تبدیل خودکار 3.1→3.0 **فقط برای ورودی تولیدکننده**؛ فایل قرارداد دست‌نخورده می‌ماند |
| اعتبارسنجی درخواست | در لبه، از روی spec (تولیدی یا `kin-openapi`) | سخت‌گیری serde را برمی‌گرداند: فیلد اجباری، enum، و `additionalProperties:false` (۲۳ مورد) |
| PostgreSQL | **pgx v5 + sqlc**؛ RLS با `set_config('kuben.org_id', …, true)` در ابتدای هر تراکنش `Tenant` | بدون ORM |
| مهاجرت | مهاجرت‌گر داخلی، دفتر-سازگار با `_sqlx_migrations` (§۶.۳) | نصب‌های موجود با swap ارتقا می‌یابند |
| صف‌ها | پکیج داخلی `pgq` با همان SQL `FOR UPDATE SKIP LOCKED` | اسکیما ثابت می‌ماند؛ River کاندید بعد از G7 |
| Kubernetes (hub) | controller-runtime + client-go + controller-gen + gateway-api + k8s.io/metrics | SSA با field manager ثابت؛ envtest |
| Kubernetes (agent) | client-go **dynamic** + SSA روی `unstructured` | سبک |
| AgentLink | `crypto/tls` + فریم‌بندی خودمان | بایت‌به‌بایت با fixture |
| بیلد | `moby/buildkit/client` (داخل Job، ADR‑0028)، `buildpacks/pack` | استریم پیشرفت |
| OCI | go-containerregistry | tag list، digest، ۴۲۹، credential helperها |
| DNS / ACME | miekg/dns، libdns، lego با import انتخابی | Cloudflare + rfc2136 + route53 در شروع |
| GitHub | go-github + ghinstallation | — |
| رمز عبور | `alexedwards/argon2id` (PHC، v=0x13، پارامترها از همان کلیدهای config) | هش‌های موجود معتبر می‌مانند |
| secret at rest | `crypto/aes` + GCM؛ DEK برای هر revision زیر KEK نسخه‌دار؛ AAD `kuben/secret/v1\|org\|secret\|rev` و `kuben/dek/v1\|…\|version`؛ قالب `nonce‖ct‖tag` | دقیقاً همان Rust؛ fixture الزامی |
| semver / glob / IDNA | **پورت مستقیم معناشناسی Rust** (انجام شد)؛ Masterminds فقط برای parse نسخهٔ تگ | دفتر جایگزینی |
| config | koanf v2 + پورت دستور زبان مقدار env در figment (انجام شد)؛ `config.Secret` با redaction | همان کلیدها، همان `KUBEN_…__…` |
| CLI | cobra | همان زیرفرمان‌ها و exit codeها |
| کش | `golang-lru/v2/expirable` | جایگزین moka |
| لاگ/متریک | `log/slog` (JSON)، `prometheus/client_golang`، pprof روی loopback | نام متریک‌ها قرارداد است (§۶) |
| تست | `testing` + go-cmp؛ testcontainers-go؛ envtest؛ golden؛ fuzz | بدون mock دیتابیس |
| کیفیت | golangci-lint v2، NilAway، govulncheck، go-licenses، gofumpt | §۱۰ |
| انتشار | goreleaser: ۵ هدف، static، `-trimpath`، SBOM، cosign | جایگزین cargo-auditable + zig |
| UI | `embed.FS` | بدون build tag |
| Go | **1.27** (toolchain پین در `go.mod`) | — |

---

## ۶. سازگاری: فهرست کامل قراردادها

### ۶.۱ ماتریس (هر ردیف یک fixture در `go/testdata/compat/` که از **باینری Rust 1.2.x** تولید شده، و یک تست)

| # | قرارداد | چه چیزی باید یکی بماند |
|---|---|---|
| ۱ | OpenAPI | همان فایل؛ oasdiff بدون breaking |
| ۲ | اسکیمای SQL و دفتر مهاجرت | ۳۴ مهاجرت موجود بدون تغییر بایت؛ §۶.۳ |
| ۳ | **JSON کانونی و هش‌های محتوانشانی** | `render::canonical`: کلیدها مرتب در هر سطح، بدون فاصله، قالب اسکالر serde_json (اعداد، escapeها بدون `<>&`). هش `sha256:` منابع و inventory در DB ذخیره است؛ اگر Go یک بایت متفاوت بنویسد، هر plan موجود «تغییرکرده» دیده می‌شود. پیاده‌سازی در `internal/wire`، با fixture از planهای واقعی و تست fuzz تفاضلی |
| ۴ | خروجی render | golden snapshotهای موجود `render/snapshots` |
| ۵ | CRD YAML | برابری ساختاری با `kuben.dev_all.yaml` شامل CEL، defaults، `x-kubernetes-*` |
| ۶ | پروتکل AgentLink | فریم‌بندی، Hello/Welcome/Refused، نسخه‌های N و N‑1، featureها، گواهی‌های کوتاه‌عمر و تمدید |
| ۷ | رمز عبور، session، توکن API | PHC argon2id؛ `sha256(id)`؛ `kbn_pat_<id>_<secret>` با مقایسهٔ زمان-ثابت |
| ۸ | secretهای sealed، توکن‌های DNS provider، secret وب‌هوک | §۵؛ fingerprint کلیدها `sha256("kuben/kek-fingerprint/v1\|"‖key)` |
| ۹ | وب‌هوک‌ها | `kuben-signature: t=…,v1=…`، `Kuben-Delivery`، `kuben-event`، بدنهٔ payload |
| ۱۰ | SSE | نام رویدادها (`snapshot`، deltaها مثل `pod_upsert`، `resync`، `error`)، `id` = seq |
| ۱۱ | خطاها | RFC 9457، کدها (`not_found`، `validation_failed`، …)، status codeها |
| ۱۲ | audit | رشته‌های action (مثل `webhook.delivery.retried`)، شکل `data` |
| ۱۳ | config | همهٔ کلیدها، پیش‌فرض‌ها، نام‌های env، ترتیب لایه‌ها (انجام و پین شد) |
| ۱۴ | CLI | زیرفرمان‌ها، flagها، exit codeها، خروجی‌های ماشینی؛ golden از `--help` |
| ۱۵ | Helm values و RBAC | values بی‌تغییر؛ فقط image |
| ۱۶ | متریک‌ها و health | نام و label متریک‌های Prometheus، مسیرهای `/healthz`/`/readyz` |
| ۱۷ | کوکی‌ها و headerها | نام کوکی session، صفات، CSP |
| ۱۸ | قفل‌های advisory و کلیدها | `domain.LockKey` و `hashtextextended(…, 5)`: بایت‌به‌بایت (انجام شد) |
| ۱۹ | export جداسازی (detach) | inventory و runbook |

### ۶.۲ تفاوت‌های ذاتی Go که باید مهار شوند
- `encoding/json` نویسه‌های `<`، `>`، `&` را escape می‌کند و serde نه → همهٔ خروجی‌های JSON از `wire.Marshal` (با `SetEscapeHTML(false)`)؛ lint `forbidigo` روی `json.Marshal` مستقیم در `api` و `platform`.
- decode پیش‌فرض Go سهل‌گیر است (فیلد ناشناخته، enum ناشناخته، فیلد اجباری غایب) → اعتبارسنجی لبه از spec + `UnmarshalText` اعتبارسنج روی همهٔ enumها (از جمله `perm.Role`، کار باقی‌مانده).
- ترتیب کلید map در Go تصادفی است → هر جا ترتیب قرارداد است، slice یا `wire.Canonical`.
- مقدار صفر struct همیشه ساختنی است → `Validate()` در مرزها.

### ۶.۳ دفتر مهاجرت
1. اگر `_sqlx_migrations` هست و دفتر Go نیست → seed در یک تراکنش با advisory lock (نسخه + checksum).
2. checksum فایل‌های embed با دفتر مقایسه می‌شود؛ اختلاف = خطای صریح.
3. **انجماد اسکیما:** تا «نقطهٔ بی‌بازگشت» (§۱۴) هیچ مهاجرت جدیدی نوشته نمی‌شود، نه در Rust نه در Go؛ این همان چیزی است که rollback به Rust را ممکن نگه می‌دارد. `_sqlx_migrations` تا دو انتشار بعد حذف نمی‌شود.

---

## ۷. بستن ضعف‌های Go نسبت به Rust

| # | ضعف | سازوکار ساختاری | gate | آنچه صادقانه می‌ماند |
|---|---|---|---|---|
| ۹ | ردپای agent | ماژول جدا، dynamic client و discovery استاندارد، بدون informer سراسری، `automemlimit`، `GOGC=50` | e2e: RSS بیکار ≤ ۴۰ MiB **سخت**؛ حجم باینری فقط هشدار | برابر Rust نمی‌شود |
| ۱۰ | GC و OOM | `GOMEMLIMIT` = ۹۰٪ limit، `requests=limits`، همهٔ بافرها کران‌دار، `MaxBytesReader`، استریم به‌جای بارگذاری | soak ۳۰ دقیقه‌ای در e2e بدون رشد یکنوا؛ pprof به‌عنوان artifact | مکث GC صفر نیست؛ برای control plane بی‌اهمیت |
| ۱۱ | nil | `opt.Val[T]`؛ DTOهای تولیدی در مرز به تایپ دامنه تبدیل می‌شوند؛ `recover` در middleware و `supervise` (با لاگ stack و incident) | **NilAway** + `nilnil` + `nilerr`؛ fuzz روی decoderها | NilAway false negative دارد |
| ۱۲ | enum جبری | sealed interface + `//sumtype:decl`؛ ماشین حالت با جدول انتقال و یک تابع `Apply`؛ قیود SQL به‌عنوان لایهٔ دوم | تست همهٔ زوج‌های (از، رویداد)؛ `gochecksumtype` | مقدار صفر ممنوع‌شدنی نیست؛ `Validate()` در مرز |
| ۱۳ | تطبیق جامع | — | `exhaustive` + `gochecksumtype`، `default` جامع حساب نمی‌شود | فقط در CI/ادیتور |
| ۱۴ | data race | مالکیت صریح؛ `errgroup`/`supervise`؛ core بدون goroutine | `-race` روی همهٔ تست‌ها؛ e2e شبانه با باینری `-race` | پویا است، نه ایستا |
| ۱۵ | مومنتوم | قراردادهای قفل، oracle، برش عمودی قابل تحویل | — | زمان واقعی است |

قواعد کامل در `go/CONVENTIONS.md`. آنچه فقط به‌خاطر Rust بود و پورت **نمی‌شود**: `kuben-crd`/`crdgen`، باینری `openapi` و annotationهای utoipa، `transport.rs`، لوله‌کشی `oci.rs`/`dns.rs`/`github.rs`، `informer.rs`/`leader.rs`/`crd_apply.rs`، traitهای تک‌پیاده‌سازی و `async-trait`، ماکروی `id_type!`، feature flagها، `duration.rs`/`time.rs`، skip بی‌صدای تست‌های PG، helperهای ساخته‌شده فقط برای راضی‌کردن `too_many_lines`.

---

## ۸. فازها

> ترتیب نسخهٔ ۱ لایه‌ای بود (همهٔ store، بعد همهٔ platform، بعد همهٔ api). در نسخهٔ ۲: **اسکلت راه‌رونده، بعد برش‌های عمودی به همان ترتیبی که محصول ساخته شد (M1…M5)**، چون سناریوهای پذیرش `tests/http.rs` هم به همین ترتیب نام‌گذاری شده‌اند و هر برش یک خروجی قابل نمایش می‌دهد.

### G0 — پایه و رفع ریسک (بخشی انجام شده)
- ✅ `go.work`، ماژول‌ها، `CONVENTIONS.md`، `.golangci.yml`، Go 1.27، turbo 2.11.2.
- ✅ تغییر نام `go/kuben` → `go/hub`؛ `experimentalGoWorkspaces` فعال.
- ✅ `go/tools` با ابزارهای پین؛ ✅ CI job `go` (golangci-lint از action رسمی، NilAway، race، govulncheck، `go mod verify`). ⬜ go-licenses.
- ✅ **Spike ۱ — OpenAPI 3.1:** ogen انتخاب شد (`go/SPIKES.md`).
- ✅ **Spike ۲ — اندازه:** hub ۵۸٫۳ MiB (فقط وابستگی‌ها)، agent ۱۰٫۴ MiB بدون discovery (`go/SPIKES.md`). ⬜ RSS در job e2e روی kind.
- ⬜ **fixtureهای سازگاری** از image رسمی 1.2.x (نه بیلد محلی)، و **oracle harness**.
- ✅ `PARITY.md`، `SUBSTITUTIONS.md`، `scripts/rust-drift.sh`.
- *خروج:* هر دو spike عدد و تصمیم دارند؛ CI سبز؛ fixtureها commit شده.

### G1 — `internal/core` (✅ پورت شد، ⬜ هنوز «تمام» نیست)
۲۳ ماژول در ۲۶ پکیج، همهٔ تست‌های Rust پورت + تست‌های پین JSON؛ `go vet` و `-race` سبز. برای بستن: مرور خط‌به‌خط خروجی عامل‌ها، اجرای golangci-lint و NilAway، `perm.Role.UnmarshalText`، یکی‌کردن `saturatingSub`/`asciiLower`/`required[T]` در `clock` و `wire`، سه تصمیم config (§۱۸).

### G2 — اسکلت راه‌رونده
یک باینری واقعی، باریک ولی از سر تا ته: مهاجرت‌گر + `Tenant`/RLS + users/sessions/orgs → login، `/me`، فهرست پروژه‌ها، SSE خالی، embed کنسول، `serve --roles=api`، image، چارت روی kind.
*خروج:* کنسول (قدیم یا جدید، هر کدام آماده بود) روی باینری Go لاگین می‌کند؛ باینری روی دیتابیسِ ساختهٔ Rust بدون مهاجرت جدید بالا می‌آید؛ goreleaser برای ۵ هدف می‌سازد؛ oracle روی این مسیرها سبز.

### S1 … S5 — برش‌های عمودی (هر برش: repoها + platform + routeها + سناریوهای پذیرش همان milestone)

| برش | دامنه | شامل |
|---|---|---|
| **S1** (M1) | هستهٔ بادوام | پروژه/محیط/اپ، release، DeploymentRun، materializer (fence، drift، write با SSA)، projection، leader، RBAC/توکن/audit، SSE واقعی |
| **S2** (M2) | image و HTTPS | OCI، دامنه‌ها و Gateway، cert، doctor، **agent + AgentLink** (با ماتریس نسخهٔ مختلط)، registry login، secretها و keyring |
| **S3** (M3) | Git → بیلد ایزوله | source/GitHub App، BuildRun، BuildKit، scan و evidence |
| **S4** (M4) | MVP عملیاتی | policy/approval، controls (freeze، pause، emergency)، incidents/webhooks، backup، upgrade journal، detach، SSO، CI trust |
| **S5** (M5) | پیش‌نمایش و UX | previews، domain claims و DNS، status page، image policy، usage/rollups، evidence graph |

*خروج هر برش:* تست‌های store و platform آن برش پورت و سبز (PG واقعی، envtest)؛ سناریوهای `m<N>_*` از `http.rs` پورت و سبز؛ oracle سبز؛ ردیف‌های `PARITY.md` «مرورشده»؛ بدون یافتهٔ lint.

### G5 — CLI، bootstrap، bundle، upgrade، backup، dns01-issuer
*خروج:* `scripts/e2e.sh` روی باینری Go سبز؛ golden خروجی CLI برابر؛ ارتقای درجا از نصب Rust (دیتابیس + agent) سبز.

### G6 — قطار انتشار و cutover مرحله‌ای (§۱۴): alpha → beta → rc → 2.0.0
### مسیر F — کنسول جدید (§۲۲)، موازی با همهٔ فازها چون قرارداد API قفل است
### G7 — حذف Rust از `main` در ورود به rc (شاخهٔ `release/1.2` می‌ماند؛ oracle از image منتشرشده استفاده می‌کند)
`crates/`، `Cargo.*`، `rust-toolchain.toml`، `clippy.toml`، `.cargo/`، nextest، taskهای cargo در `turbo.json`، zig/cargo-auditable در release، cargo-audit در security؛ ADR‑0033 (Go runtime) و به‌روزرسانی ADR‑0024؛ مستندات سایت و `docs/`.

---

## ۹. روش اجرا

### ۹.۱ موازی‌سازی (در G1 آزموده شد)
ماژول‌ها بر اساس **گراف وابستگی** به گروه‌های بسته تقسیم می‌شوند؛ هر گروه یک عامل موازی با: `CONVENTIONS.md`، پکیج‌های نمونه، فهرست دقیق فایل‌های Rust، ممنوعیت `go get`/`go mod tidy` و دست‌زدن به پکیج دیگران، و گزارش پایانی اجباری (تعداد تست Rust/Go، انحراف‌ها، چیزهای محلی که جای دیگری تعلق دارند). وابستگی‌ها را یک نفر، از پیش، اضافه می‌کند.

### ۹.۲ دروازهٔ مرور (جدید)
خروجی عامل «پورت‌شده» است، نه «تمام». «تمام» یعنی: مرور خط‌به‌خط در برابر فایل Rust، lint و NilAway سبز، انحراف‌ها یا پذیرفته و در `SUBSTITUTIONS.md`/`PARITY.md` ثبت شده یا اصلاح شده.

### ۹.۳ شاخه‌ها و commitها
شاخهٔ محلی `feat/go-rewrite` از `main`؛ یک commit برای هر پکیج/برش با پیام انگلیسی؛ هیچ push بدون دستور. Rust روی `main` فقط رفع باگ و امنیت می‌گیرد (شاخهٔ نگهداری `release/1.2`).

### ۹.۴ همگام‌سازی با Rustِ در حال تغییر (جدید)
از زمان شروع پورت، ۷ فایل Rust تغییر کرده (نصب‌کننده و `serve`)؛ `kuben-core` و قراردادها تغییر نکرده‌اند. رویه: `PARITY.md` برای هر فایل **commit مبنا** را نگه می‌دارد؛ اسکریپت `scripts/rust-drift.sh` فایل‌هایی را که بعد از مبنایشان تغییر کرده‌اند فهرست می‌کند؛ هیچ برشی بسته نمی‌شود مگر drift آن صفر باشد. توصیه: تا cutover، فیچر جدید در Rust نرود.

---

## ۱۰. کیفیت و CI

- **قالب:** gofumpt، goimports.
- **lint (شکست build):** errcheck (با type assertion و blank)، govet کامل، staticcheck، errorlint، nilerr، nilnil، **NilAway**، exhaustive، gochecksumtype، gosec، bodyclose، noctx، contextcheck، rowserrcheck، sqlclosecheck، musttag، recvcheck، funlen ۱۰۰، gocyclo ۱۵، gocritic، revive، depguard (بدون ORM؛ core بدون IO)، forbidigo (`time.Now`، `fmt.Print*`، `panic`، `json.Marshal` مستقیم در api/platform).
- **تست:** جدولی، پکیج بیرونی، `-race -shuffle=on` همیشه؛ PG واقعی؛ envtest؛ golden؛ fuzz شبانه روی decoderها، semver، glob، IDNA، env-value، JSON کانونی.
- **پوشش:** کف ۸۵٪ برای `core` و `wire`، ۷۵٪ کل؛ مهم‌تر از درصد: **هیچ تست Rust بدون همتا** (`PARITY.md`).
- **زنجیرهٔ تأمین:** `go mod verify` با sumdb روشن در CI (در sandbox محلی به‌اجبار خاموش است)، govulncheck، go-licenses (هم‌راستا با ADR‑0012)، SBOM، cosign، ابزارهای پین در `go/tools`.
- **کارایی:** benchmarkهای `render`، `authz.Check`، JSON کانونی و مسیرهای فهرست؛ مقایسهٔ p95 با oracle در e2e.

---

## ۱۱. بودجه‌ها

| معیار | Rust 1.2.x | Go | نوع |
|---|---|---|---|
| باینری hub (stripped، linux/amd64) | بودجهٔ فعلی CI: ۴۵ MiB | هشدار در **۷۵ MiB** (Spike ۲: وابستگی‌ها به‌تنهایی ۵۸٫۳ MiB) | هشدار |
| باینری agent | — | هشدار در ۳۵ MiB (Spike ۲: با discovery استاندارد ۲۵٫۴ MiB) | هشدار |
| RSS بیکار agent | ۵ تا ۱۵ MiB | ≤ ۴۰ MiB | **سخت** |
| RSS بیکار hub با ۲۰۰ pod | FOOTPRINT-RUNBOOK | ≤ ۱٫۵× Rust با `GOMEMLIMIT` | **سخت** |
| p95 مسیرهای فهرست و deploy | oracle | ≤ ۱٫۲× Rust | **سخت** |
| زمان راه‌اندازی تا ready | oracle | ≤ Rust | هشدار |
| image | بودجهٔ فعلی CI: ۵۰ MiB | بازنگری پس از Spike ۲ | هشدار |
| بیلد release در CI | ~۱۵ دقیقه | < ۲ دقیقه | هدف |
| باندل کنسول | ۱۲۵/۲۰۰ kB | بی‌تغییر | سخت (موجود) |

طبق تصمیم شما (۰۹‑۲۱) **حجم باینری hub و agent هیچ‌کدام gate سخت نیست**؛ درستی و کتابخانه‌های استاندارد مقدم‌اند. آنچه روی سرور مشتری دیده می‌شود (agent، RSS، تأخیر) سخت می‌ماند.

---

## ۱۲. Turborepo

`turbo` ≥ 2.11.2 (2.10 پرچم را نمی‌شناسد؛ ارتقا انجام شد). `experimentalGoWorkspaces` بعد از تغییر نام به `go/hub` روشن می‌شود (نام `kuben` با crate برخورد دارد؛ نام‌ها: `hub`، `agent`، `kubenapi`، `tools`). بازتعریف با `experimentalTaskCommand`: `test` → `go test -race -shuffle=on ./...`؛ `lint` → golangci-lint + nilaway؛ `hub#build` → `go build -trimpath -o bin/kuben ./cmd/kuben` با `dependsOn: ["@kuben/console#build"]`؛ `gen` → `go generate ./...` با outputs روی CRD YAML و کد تولیدی. چون پرچم تجربی است: نسخه پین، و هر task معادل ساده در `Makefile` دارد تا CI به Turborepo وابسته نباشد.

---

## ۱۳. بهبودهای مجاز در حین بازنویسی (فهرست بسته)

1. OpenAPI spec-اول با سرور strict و **اعتبارسنجی درخواست از روی spec**.
2. بیداری materializer با `LISTEN/NOTIFY`؛ polling فقط پشتیبان.
3. envtest در CI به‌جای kind برای materializer و CEL.
4. DNS‑01 چندprovider با lego.
5. credential helperهای رجیستری (ECR/GCR/ACR).
6. استریم پیشرفت بیلد به SSE.
7. pprof روی loopback؛ `GOMEMLIMIT` در چارت.
8. `-race` و fuzz در CI.
9. انتشار بازتولیدپذیر با SBOM و امضا.
10. تست‌های جدولی و فایل‌های کوچک.
11. **redaction secretهای config** (`config.Secret`؛ در Rust رشتهٔ ساده بودند) — انجام شد.
12. تست‌های PG که دیگر محلی بی‌صدا skip نمی‌شوند.

---

## ۱۴. قطار انتشار و cutover

**قاعده:** هیچ‌چیز مستقیم 2.0.0 نمی‌شود. هر ایستگاه معیار ورود و خروج دارد و فقط با پاس‌شدن خروج، ایستگاه بعد باز می‌شود. `latest` و کانال پایدار تا GA روی 1.2.x می‌مانند؛ پیش‌انتشارها فقط با تگ صریح نصب می‌شوند (goreleaser و چارت: `version`/`appVersion` = همان تگ، `prerelease: auto`).

| ایستگاه | چه زمانی | برای چه کسی | معیار خروج (همه الزامی) |
|---|---|---|---|
| **2.0.0‑alpha.N** | بعد از G2 و بعد از هر برش S1…S3 | فقط توسعه؛ دیتابیس تازه یا **کپی** دیتابیس 1.2 | CI سبز؛ oracle روی برش‌های پورت‌شده سبز؛ فهرست «هنوز نیست» در یادداشت انتشار؛ بدون تضمین ارتقا |
| **2.0.0‑beta.N** | فیچر-کامل: S1…S5 + G5 + کنسول جدید | staging و داوطلبان | همهٔ سناریوهای پذیرش پورت و سبز؛ ارتقای درجا از 1.2.x **و برگشت** سبز؛ ماتریس agent/hub سبز؛ gateهای §۱۱ (RSS، p95) سبز؛ soak ۳۰ دقیقه‌ای؛ صفر باگ P0/P1 باز؛ دو beta پیاپی بدون رگرسیون قرارداد |
| **2.0.0‑rc.N** | انجماد کد: فقط رفع باگ؛ Rust از `main` حذف | staging واقعی، تمرین cutover | تمرین کامل C0→C3 و rollback مستندشده؛ مرور امنیتی (gosec، govulncheck، مسیر secretها، CSP)؛ مستندات و راهنمای ارتقا کامل؛ **۷ روز** اجرا بدون P0/P1؛ هر رفع باگ = rc بعدی و شروع دوبارهٔ شمارش |
| **2.0.0** | همان commit آخرین rc، فقط تگ دوباره (بدون بیلد مجدد متفاوت) | همه | — |

**cutover نقش‌به‌نقش** (در beta تمرین، در rc تکرار، در GA اجرا):

| مرحله | چه چیزی Go می‌شود | rollback |
|---|---|---|
| C0 | سایه: Go با `--roles=api` فقط‌خواندنی کنار Rust؛ مقایسهٔ پاسخ‌ها | خاموش‌کردن |
| C1 | نقش `api` | برگرداندن image |
| C2 | نقش `controller` (Lease و fencing در SQL مانع دو نویسنده‌اند) | برگرداندن image |
| C3 | agentها، کلاستر به کلاستر (hub نسخه‌های N و N‑1 را می‌پذیرد) | برگرداندن agent همان کلاستر |

**انجماد اسکیما تا بعد از GA:** 2.0.0 **هیچ مهاجرت جدیدی ندارد**؛ اولین مهاجرت (`0035`) در 2.1.0 می‌آید. نتیجه: حتی بعد از GA، برگشت به 1.2.x فقط برگرداندن image است. نقطهٔ بی‌بازگشت = 2.1.0، با preflight backup خودکار (M4.8). ماتریس پشتیبانی: hub 2.0 ↔ agent 1.2 و 2.0؛ 1.2.x تا شش ماه بعد از GA رفع امنیتی می‌گیرد (`release/1.2`).

---

## ۱۵. ریسک‌ها

| ریسک | احتمال | مهار |
|---|---|---|
| تولیدکنندهٔ OpenAPI با 3.1 کنار نیاید | **بالا** | Spike ۱ در G0؛ راه پشتیبان تبدیل 3.1→3.0 فقط برای تولید |
| JSON کانونی یک بایت متفاوت → هش planها عوض شود | متوسط، اثر بالا | `internal/wire` با fixture از planهای واقعی + fuzz تفاضلی؛ در S1 قبل از materializer |
| drift رفتاری نامرئی | بالا بدون مهار | oracle، پورت ۱:۱ تست‌ها، دفتر جایگزینی، دروازهٔ مرور |
| کتابخانهٔ جایگزین رفتار دیگری داشته باشد | **رخ داد** (semver، IDNA) | اصل ۳؛ پورت معناشناسی وقتی کتابخانه نمی‌خواند |
| Rust در حین پورت تغییر کند | **در حال رخ‌دادن** | §۹.۴ |
| ناسازگاری at-rest | متوسط | ماتریس §۶ + ارتقا/برگشت درجا در CI |
| CRD YAML متفاوت | متوسط | برابری ساختاری |
| نسخهٔ مختلط agent/hub | متوسط | ماتریس CI؛ cutover C3 |
| جدول‌های Unicode متفاوت در IDNA | کم | پین نسخهٔ x/net؛ تست روی دامنه‌های موجود در DB پیش از C1 |
| RSS/تأخیر از gate بیرون بزند | متوسط | بافرهای کران‌دار؛ کش محدود؛ `GOMEMLIMIT` |
| Turborepo Go تجربی بشکند | متوسط | پین + Makefile |
| «سیستم دوم» | بالا | فهرست بستهٔ §۱۳ |
| ابزارهای lint در sandbox محلی نصب نیستند | قطعی | CI مرجع است؛ هیچ برشی بدون CI سبز بسته نمی‌شود |

---

## ۱۶. تعریف «تمام شد»

**هر PR/برش:** gofumpt؛ golangci-lint و NilAway سبز؛ `go test -race` با PG؛ envtest (platform)؛ `sqlc vet`؛ govulncheck و licenses؛ فایل‌های تولیدی بدون diff؛ oracle سبز؛ `PARITY.md` و `SUBSTITUTIONS.md` به‌روز؛ drift صفر؛ مستندات به‌روز؛ commit محلی.
**cutover:** همهٔ بالا + e2e kind، ارتقا **و برگشت** درجا، ماتریس نسخهٔ مختلط، gateهای §۱۱، `check-api-compat.sh` علیه `main`، Playwright و axe کنسول جدید روی **هر دو** باینری سبز، smoke روی ۵ هدف، `helm lint --strict`.

---

## ۱۷. پیشنهادهای طلایی

1. **قراردادها را قفل کنید** — و فهرستشان را کامل بدانید: ۱۹ ردیف §۶، نه فقط API و SQL.
2. **JSON کانونی را اول بسازید.** هش planها در دیتابیس است؛ این تنها جایی است که «تقریباً یکسان» یعنی شکستن همهٔ نصب‌ها.
3. **باینری Rust داور است** تا آخر؛ fixtureها از image رسمی.
4. **هر کتابخانهٔ جایگزین را متهم فرض کنید** و در دفتر ثبت کنید (دو مورد واقعی در همین هفته).
5. **اسکلت راه‌رونده قبل از عمق.** یک login واقعی روی kind در G2 از ده پکیج کامل بدون باینری ارزشمندتر است.
6. **cutover نقش‌به‌نقش** با انجماد اسکیما؛ rollback ارزان حتی بعد از GA (نقطهٔ بی‌بازگشت = 2.1.0).
7. **spike تولیدکنندهٔ OpenAPI 3.1 و spike RSS پیش از تعهد**؛ هر دو با عدد تصمیم می‌گیرند.
8. **agent سبک بماند**؛ تنها چیزی است که روی سرور مشتری نصب می‌شود.
9. **مرور، بعد از عامل.** پورت موازی سریع است ولی «سبز بودن تست» مساوی «درست بودن» نیست.
10. **Rust را منجمد کنید.** هر commit فیچر در Rust، بدهی پورت است.
11. **اعتبارسنجی لبه از روی spec** سخت‌گیری serde را یک‌جا برمی‌گرداند، به‌جای صد decoder دستی.
12. **parity اول**؛ فهرست بهبودها بسته است.
13. **alpha → beta → rc → GA**، و GA همان بایت‌های آخرین rc است؛ 2.0.0 مهاجرت جدید ندارد تا برگشت همیشه ممکن باشد.
14. **موتور و ظاهر را هم‌زمان عوض نکنید:** کنسول shadcn اول روی Rust (1.3.0)، بعد موتور Go (2.0).
15. **کش Kubernetes را از روز اول محدود کنید** (strip managedFields، label selector)؛ بزرگ‌ترین اهرم حافظه است و بعداً گران اصلاح می‌شود.
16. **حذف از قرارداد فقط با تأیید شما؛** حذف کد داخلی فقط با شاهد و ثبت در `PARITY.md`.

---

## ۱۸. تصمیم‌ها

**بسته‌شده (پیش‌فرض‌هایی که اعلام شد و مخالفتی نبود):** نسخهٔ cutover 2.0.0 · agent باینری جدا · DNS‑01 اولیه: cloudflare، rfc2136، route53 · module path `github.com/Teamtem-dev/kuben/go/{hub,agent,kubenapi,tools}` · Go 1.27 · اندازهٔ hub فقط هشدار.

**باز (با تأیید نهایی شما بسته می‌شوند):**
1. **انجماد فیچر در Rust تا cutover** (پیشنهاد: بله؛ فقط رفع باگ و امنیت).
2. **ترتیب جدید فازها** (اسکلت + برش‌های عمودی) به‌جای لایه‌ای (پیشنهاد: بله).
3. **cutover مرحله‌ای C0…C3** (پیشنهاد: بله؛ اگر محیط staging ندارید، C0 حذف و C1/C2 یک‌جا).
4. سه مورد config: پذیرش عدد/بولین بدون نقل‌قول برای فیلد متنی در env (پیشنهاد: بماند)؛ `sso.client_secret=""` یعنی تنظیم‌نشده (پیشنهاد: بله)؛ متن خطای decode از mapstructure (پیشنهاد: بپذیریم).
5. شاخهٔ کار: `feat/go-rewrite` از `main`، و ماندن تغییر turbo 2.11.2 در همان شاخه (پیشنهاد: بله).
6. **قطار انتشار** §۱۴ با rc هفت‌روزه و **بدون مهاجرت جدید در 2.0.0** (پیشنهاد: بله).
7. **کنسول جدید به‌صورت 1.3.0 روی Rust** پیش از 2.0 (پیشنهاد: بله)، و شاخهٔ جدا `feat/console-shadcn`.
8. **کاندیدهای هرس** §۱۹: تأیید ردیف‌به‌ردیف؛ به‌ویژه SQLite نصب‌کننده (پیشنهاد: بماند) و سیاست حذف از قرارداد (پیشنهاد: فقط deprecate در 2.0).
9. **lefthook** برای git hookها (پیشنهاد: بله).

---

## ۱۹. هرس: حذف کد و فیچر اضافی، بدون شکستن چیزی

**قاعدهٔ پورت:** چیزی پورت می‌شود که دست‌کم یکی را داشته باشد: (الف) نقطهٔ ورود کاربر (عملیات API که کنسول، CLI یا مستندات از آن استفاده می‌کند)، (ب) دادهٔ ذخیره‌شده‌ای که به آن وابسته است، (ج) ADR که آن را الزام می‌کند. بقیه کاندید حذف است.

**دو نوع حذف، دو قاعده:**
- **کد داخلی** (بدون اثر روی قرارداد): با شواهد حذف می‌شود و در `PARITY.md` با برچسب `dropped` و دلیل ثبت می‌شود.
- **سطح قرارداد** (عملیات API، کلید config، زیرفرمان CLI): فقط با تأیید صریح شما، فقط چون 2.0 نسخهٔ major است، با ثبت در changelog و allowlist در `check-api-compat.sh`. پیش‌فرض: در 2.0 فقط `deprecated`، حذف در 3.0.

**ممیزی در G0:** اسکریپت `scripts/api-usage-audit.sh` هر ۱۲۹ عملیات را با استفاده در کنسول، CLI، e2e و مستندات تطبیق می‌دهد و فهرست «بی‌مصرف» می‌دهد؛ تصمیم با شما.

| کاندید | شاهد از ریپو | پیشنهاد |
|---|---|---|
| نقش `activator` | stub ۲۲ خطی، هیچ کاری نمی‌کند، feature flag جدا و task lint جدا دارد | پورت نشود؛ نام نقش در parser config پذیرفته و با هشدار نادیده گرفته شود تا config کسی نشکند |
| `ScopeChain.aliases` و بقایای واردکنندهٔ 1.x | کامنت خود کد: «Empty in practice: 1.x data is not carried over»؛ فقط یک جا مقداردهی غیرخالی (در تست) | حذف، پس از تأیید با یک query که `role_bindings` با UID قدیمی ندارند |
| `LeaderElector`/`NoopLeader` | trait تک‌پیاده‌سازی | حذف شد (controller-runtime) |
| مدل قدیمی `AppRelease` | ۹ استفاده؛ احتمالاً با `Release` در ADR‑0026 هم‌پوشان | بررسی در S1؛ اگر فقط مسیر سازگاری است حذف |
| `SubjectKind::Team` | یک استفاده؛ تیم‌ها پیاده نشده‌اند | مقدار رشته‌ای بماند (قید SQL)، مسیر کدی ساخته نشود |
| کد تکراری پورت | `saturatingSub` ×۳، `asciiLower` ×۲، `required[T]` ×۲ | یکی در `clock`/`wire` |
| `m0-spikes.yml`، `docs/archive`، اسنپ‌شات‌های schemars | مربوط به دورهٔ M0 و Rust | حذف در G7 |
| تشخیص/بازنشانی SQLite در نصب‌کننده | ۲۳ ارجاع؛ **کار اخیر خود شما** (PR #41) | بماند مگر بگویید نه |
| `local_agent`، templates، support bundle، backup، upgrade journal | نقطهٔ ورود و ADR دارند | بمانند |

**نگهبان دائمی:** `unused`، `unparam`، `dupl` و `deadcode` (golang.org/x/tools) در CI؛ کد مرده وارد نمی‌شود.

---

## ۲۰. کد تمیز، hookها و انضباط وابستگی

**ساختار:** interface کوچک و در سمت مصرف‌کننده؛ تزریق از راه سازنده (بدون فریم‌ورک DI، بدون متغیر سراسری، بدون `init()`)؛ مرزهای `internal/` با `depguard` اجبار می‌شوند؛ هر پکیج یک مسئولیت و یک doc comment؛ فایل‌ها حدود ۴۰۰ خط و توابع ≤ ۱۰۰ خط؛ خطا با `%w` و کد پایدار؛ `context.Context` اول؛ لاگ ساخت‌یافته با `slog` و کلیدهای ثابت؛ middleware HTTP به‌صورت زنجیرهٔ استاندارد `func(http.Handler) http.Handler` (request-id، recover، auth، authz، audit، limit)؛ چرخهٔ عمر سرویس‌ها با `supervise` (start/stop مرتب، shutdown باوقار با مهلت).

**git hookها با lefthook** (یک باینری، مناسب monorepo؛ الان هیچ hookی در ریپو نیست):
- `pre-commit` (زیر ۱۰ ثانیه، فقط فایل‌های staged): gofumpt + goimports، `golangci-lint --new-from-rev=HEAD`، biome، بررسی `go mod tidy`، جلوگیری از commit فایل‌های تولیدیِ کهنه.
- `commit-msg`: قالب Conventional Commits که ریپو همین حالا دارد (`feat(scope): …`).
- `pre-push`: `go test -race -short ./...` و `tsc`. (قاعدهٔ «بدون push بدون دستور شما» سر جایش است.)
- hookها میان‌بر CI نیستند: CI مرجع است.

**`go generate` به‌عنوان hook تولید:** controller-gen، sqlc، تولیدکنندهٔ OpenAPI؛ CI با `git diff --exit-code` کهنگی را می‌گیرد.

**انضباط وابستگی:** اول stdlib؛ هر وابستگی مستقیم یک ردیف در `go/SUBSTITUTIONS.md` (چرا، جایگزین چه چیزی، لایسنس)؛ بدون کتابخانهٔ «کمکی عمومی» (lo، testify، viper)؛ `go mod why` برای هر وابستگی سنگین در G0.

---

## ۲۱. مهندسی کارایی: نزدیک‌شدن به Rust تا جای ممکن

**انتظار صادقانه:** مسیرهای API و reconcile به دیتابیس و Kubernetes بسته‌اند، نه CPU؛ هدف p95 ≤ ۱٫۲× Rust واقع‌بینانه است. مسیرهای CPU-محور (render، JSON کانونی، هش) ممکن است ۱٫۵ تا ۲ برابر کندتر باشند ولی در مقیاس میلی‌ثانیه‌اند. حافظه برابر نمی‌شود (§۷).

| اهرم | کار مشخص |
|---|---|
| **PGO** | `default.pgo` از پروفایل soak در e2e، کنار `cmd/kuben`؛ معمولاً ۲ تا ۱۰٪ CPU رایگان |
| **کش controller-runtime** (بزرگ‌ترین اهرم RSS) | `DefaultTransform: TransformStripManagedFields`؛ کش محدود با `cache.ByObject` و label `app.kubernetes.io/managed-by=kuben`؛ watch فقط-metadata هر جا بدنه لازم نیست |
| **PostgreSQL** | پروتکل دودویی pgx و کش statement؛ اندازهٔ pool متناسب؛ `Batch`/pipeline در تراکنش‌های چندکوئری؛ `CopyFrom` برای rollupها؛ sqlc مانع N+1 پنهان می‌شود |
| **JSON** | encoder/decoder تولیدی و بدون reflection در مسیر داغ (امتیازی برای `ogen` در Spike ۱)؛ پرهیز از `map[string]any` جز در مرز manifest؛ نویسندهٔ کانونی به‌صورت استریم |
| **SSE** | یک broadcaster؛ فریم یک‌بار serialize و بین مشترک‌ها مشترک؛ بافر حلقه‌ای کران‌دار برای هر مشترک |
| **فایل‌های استاتیک** | فشرده‌سازی brotli/gzip در زمان بیلد و embed؛ `ETag` و `immutable` برای assetهای هش‌دار |
| **GC** | `GOMEMLIMIT` خودکار؛ `GOGC` پیش‌فرض برای hub و ۵۰ برای agent؛ بدون `sync.Pool` مگر pprof بگوید |
| **انضباط تخصیص** | benchmark با `-benchmem` برای render، authz، JSON کانونی، مسیرهای فهرست؛ `benchstat` در CI در برابر baseline؛ رگرسیون > ۱۰٪ = شکست |
| **راه‌اندازی** | مقداردهی تنبل؛ بررسی مهاجرت و گرم‌کردن کش موازی |

---

## ۲۲. مسیر F — بازسازی کامل کنسول با shadcn/ui

**وضعیت فعلی:** React 19، Vite 8، Tailwind 4، TanStack Router/Query، کلاینت OpenAPI؛ همهٔ کامپوننت‌ها دست‌ساز در **یک فایل** `components/ui.tsx`؛ حدود ۵۳۰۰ خط TSX. لایهٔ داده خوب است و می‌ماند؛ لایهٔ نمایش از نو ساخته می‌شود.

**اصل‌ها**
1. **کامپوننت‌های خام shadcn/ui**: کل مجموعه با CLI رسمی در `src/components/ui/` نصب می‌شود، **بدون سفارشی‌سازی ظاهری**؛ همهٔ رنگ و شعاع و فاصله از متغیرهای CSS در یک فایل `theme.css` می‌آید تا شما فقط با عوض‌کردن tokenها استایل بدهید.
2. **چیدمان و تجربه به سبک Dokploy**: sidebar جمع‌شونده با سوییچر سازمان/پروژه، هدر با breadcrumb، پالت فرمان (⌘K)، منوی کاربر، تغییر تم و زبان؛ صفحهٔ اپ با تب‌ها (General، Environment، Domains، Deployments، Logs، Monitoring، Advanced)؛ داشبورد خانه با کارت‌های خلاصه، نمودار مصرف، استقرارهای اخیر و رخدادهای باز. Dokploy خودش روی shadcn ساخته شده، پس shadcn خام بیشترِ آن حس را می‌دهد.
3. **clean-room (ADR‑0012):** Dokploy فقط مرجع چیدمان و UX است؛ هیچ کد، asset یا متنی از آن برداشته نمی‌شود.
4. **قرارداد API قفل است** → این مسیر کاملاً موازی با بازنویسی Go پیش می‌رود و روی **هر دو** باینری کار می‌کند.

**پشته (افزوده‌ها):** shadcn/ui (Radix، `class-variance-authority`، `tailwind-merge`، `clsx`، `tw-animate-css`)، `lucide-react`، `react-hook-form` + `zod` + `@hookform/resolvers`، `@tanstack/react-table`، `sonner`، `cmdk`، `recharts` (chart در shadcn)، `vaul`، `react-day-picker`، `react-resizable-panels`، `input-otp`، `embla-carousel-react`.

**ساختار**
```
src/components/ui/        shadcn خام (دست نمی‌خورد، جز وصلهٔ RTL مستند)
src/components/app/       ترکیب‌ها: AppSidebar، PageHeader، DataTable، ConfirmDestructive، StatusBadge، LogViewer، UsageChart
src/features/<domain>/    api.ts (queryOptions و mutationها)، schema.ts (zod)، components/
src/routes/               فقط سیم‌کشی صفحه
src/lib/                  i18n (کاتالوگ EN/FA موجود)، prefs، live (SSE)، format
```
**قواعد hook در React:** هیچ `fetch` در کامپوننت؛ هر query/mutation یک `queryOptions` یا hook در `features/*/api.ts`؛ فرم‌ها فقط با RHF + zod (schema هم‌راستا با تایپ‌های OpenAPI)؛ `useLiveUpdates` (SSE) می‌ماند؛ state سراسری فقط prefs.

**قیدهایی که باید از روز اول رعایت شوند**
| قید | راه‌حل |
|---|---|
| EN/FA و RTL | `DirectionProvider` در Radix؛ کلاس‌های منطقی Tailwind (`ms/me/ps/pe/start/end`)؛ shadcn خام در چند جا `left/right` دارد → یک codemod مستند آن‌ها را منطقی می‌کند (تنها وصلهٔ مجاز روی `ui/`) |
| WCAG 2.1 AA | Radix بیشترش را می‌دهد؛ axe در Playwright برای هر صفحه، در هر دو زبان و هر دو تم |
| CSP `style-src 'self'` (فعلی، بدون `unsafe-inline`) | prop `style` در React از CSSOM می‌گذرد و مجاز است؛ **`chart` در shadcn تگ `<style>` تزریق می‌کند** → جایگزینی با متغیرهای CSS از راه CSSOM؛ تست CSP در Playwright که هر نقض را شکست حساب کند |
| بودجهٔ باندل | lazy-loading سطح route؛ بودجهٔ جدید: JS اولیه ≤ ۲۰۰ kB brotli، هر route تنبل ≤ ۸۰ kB، نمودارها chunk جدا |
| تم | روشن/تیره با tokenها؛ بدون رنگ hard-code |

**فازها**
| فاز | محتوا | خروج |
|---|---|---|
| F0 | `shadcn init`، نصب کل مجموعه، `theme.css`، پوسته (sidebar، هدر، ⌘K، تم، زبان)، codemod RTL، تست CSP | پوسته در EN/FA، روشن/تیره، axe سبز |
| F1 | login، setup، account، projects، project، environment، app (با تب‌ها)، deployments، logs | هم‌ارزی کارکردی با کنسول فعلی در این صفحه‌ها |
| F2 | incidents، webhooks، domains، previews، status page، detached، image policy، usage، doctor، team، tokens، audit | هم‌ارزی کامل؛ حذف `components/ui.tsx` قدیمی |
| F3 | داشبورد خانه، DataTable با مرتب‌سازی/فیلتر/صفحه‌بندی، skeleton و empty stateها، بازنویسی Playwright + axe، snapshot بصری | بودجهٔ باندل، a11y و CSP سبز |

**انتشار کنسول:** پیشنهاد: به‌محض پایان F3 به‌صورت **1.3.0 روی backend فعلی Rust** منتشر شود (تغییر backend ندارد، پس با انجماد فیچر Rust تناقضی ندارد). کاربران بهبود ظاهری را زود می‌گیرند و 2.0 فقط موتور را عوض می‌کند، نه موتور و ظاهر را هم‌زمان.

---

## پیوست الف — وضعیت اجرا در ۲۰۲۶‑۰۹‑۲۱ (همه commit محلی، هیچ push)

**شاخهٔ `feat/go-rewrite`** (از `origin/main` در `86ce940`):
| commit | محتوا |
|---|---|
| `148c0ec` | turbo 2.11.2؛ `bun.lock` بدون آدرس رجیستری |
| `25b7bcb` | `go.work`، سه ماژول، `CONVENTIONS.md`، `.golangci.yml`، `PARITY.md`، `SUBSTITUTIONS.md`، `rust-drift.sh` |
| `0863432` | پورت کامل `kuben-core` (۲۶ پکیج) |
| `7ada682` | `internal/wire`، `ascii`، `clock.SaturatingSub`؛ `perm.Role` سخت‌گیر؛ رفع یافته‌های NilAway؛ `go/tools` |
| `447009d` | Go در Turborepo (`hub`، `agent`، `kubenapi`، `tools`)؛ job `go` در CI و گروه تغییر `go` |
| `529a42f`، `b914ea5` | نتیجهٔ spikeها (`go/SPIKES.md`)؛ discovery استاندارد در agent |
| `8bcc74a` | fixtureهای سازگاری از Rust + `wire.Canonical` بایت‌به‌بایت برابر Rust |

**شاخهٔ `feat/console-shadcn`** (worktree در `.cache/wt/console-shadcn`): F0 کامل — ۶۱ کامپوننت shadcn خام، `theme.css`، codemod RTL، پوستهٔ جدید (sidebar، breadcrumb، ⌘K، تم، زبان)، صفر نقض CSP بدون `unsafe-inline`، lazy routes (JS اولیه ۱۶۲ kB از ۲۰۰)، `cn` با clsx + tailwind-merge.

**G2 (۰۹‑۲۱):** ✅ کد کامل — store (مهاجرت‌گر سازگار با `_sqlx_migrations`، RLS، هویت)، API (setup، login، logout، me، رمز، پروژه‌ها، سلامت، audit، SSE)، `kuben serve`، باینری ۱۵ MiB در ۱۲ ثانیه، job oracle در CI در برابر Rust 1.2.0. ⬜ اجرای تست‌های دیتابیس و oracle (نیاز به CI یا PostgreSQL بیرون از sandbox).

**G0:** ✅ همه، جز oracle harness (با اولین مسیرهای G2 نوشته می‌شود) و اندازه‌گیری RSS (در job e2e روی kind).

**محدودیت‌های sandbox محلی (نه پروژه):** PostgreSQL محلی اجرا نمی‌شود (sandbox `shmget` را مسدود می‌کند) → تست‌های دیتابیس با `KUBEN_TEST_PG_URL` در CI اجرا می‌شوند و CI با `KUBEN_REQUIRE_PG=1` هر skip را شکست حساب می‌کند؛ golangci-lint و staticcheck محلی ساخته نمی‌شوند (sandbox نوشتن `.vscode`/`.gitmodules` را مسدود می‌کند) → در CI از action رسمی؛ ماژول‌ها از آینهٔ `file://` پر می‌شوند و بررسی checksum در CI انجام می‌شود.

## پیوست ب — تغییرات نسخهٔ ۲

| تغییر | چرا |
|---|---|
| فازها: لایه‌ای → اسکلت + برش‌های عمودی M1…M5 | کشف زودهنگام مشکلات یکپارچه‌سازی؛ خروجی قابل نمایش در هر برش |
| cutover نقش‌به‌نقش + انجماد اسکیما + نقطهٔ بی‌بازگشت | rollback ارزان؛ نسخهٔ ۱ فقط «یک انتشار» داشت |
| قرارداد ۳: JSON کانونی و هش plan | در نسخهٔ ۱ دیده نشده بود؛ اثرش شکستن همهٔ planهای موجود است |
| Spike OpenAPI 3.1 | نسخهٔ ۱ oapi-codegen را قطعی فرض کرده بود؛ spec 3.1 است |
| دفتر جایگزینی + اصل ۳ | دو ناسازگاری واقعی در G1 (semver، IDNA) |
| AgentLink = mTLS خام با فریم طول‌دار | نسخهٔ ۱ WebSocket را محتمل دانسته بود |
| ماتریس سازگاری از ۱۲ به ۱۹ ردیف | SSE، CLI، متریک، کوکی، Helm، قفل‌ها، config |
| اعتبارسنجی لبه از spec؛ `internal/wire` | سهل‌گیری decode و HTML-escape در Go |
| `go/hub` به‌جای `go/kuben`؛ turbo ≥ 2.11.2 | برخورد نام در Turborepo؛ پرچم در 2.10 نبود |
| ماژول `go/tools`، sumdb در CI، go-licenses، کف پوشش، benchmark | استاندارد حرفه‌ای زنجیرهٔ تأمین و کارایی |
| دروازهٔ مرور و `PARITY.md` | خروجی عامل ≠ تمام |
| رویهٔ drift با Rust | Rust بعد از شروع پورت ۷ فایل تغییر کرده |
| gateهای تأخیر p95 و زمان راه‌اندازی | «بهینه» باید اندازه‌پذیر باشد |
| `secrecy` در Rust استفاده نمی‌شد | اصلاح فرض نسخهٔ ۱؛ redaction حالا بهبود ۱۱ است |
| **۲٫۱:** قطار انتشار؛ 2.0.0 بدون مهاجرت جدید | درخواست شما؛ rollback حتی بعد از GA |
| **۲٫۱:** هرس (§۱۹)، کد تمیز و lefthook (§۲۰)، کارایی با PGO و کش محدود (§۲۱) | درخواست شما |
| **۲٫۱:** مسیر F، کنسول shadcn به سبک Dokploy، انتشار 1.3.0 روی Rust | درخواست شما؛ معیار «کنسول بدون تغییر» با «قرارداد API بدون تغییر» جایگزین شد |
