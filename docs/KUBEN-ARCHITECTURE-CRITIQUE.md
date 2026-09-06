# 🔍 نقد ساختاری و معماری سند طلایی Kuben — و اصلاحات پلن نهایی

> بازبینی انتقادی `KUBEN-GOLDEN-ARCHITECTURE.md` (v1.0) از نگاه یک منتقد بیرونی: کجاها خوش‌بینانه است، کجاها پیچیدگی بی‌دلیل دارد، کدام ریسک‌ها دست‌کم گرفته شده‌اند، و پلن نهایی (v1.1) دقیقاً چه تغییری می‌کند.

| | |
|---|---|
| **نسخه** | 1.0 (نقد) → تولید v1.1 پلن |
| **تاریخ** | 2026-09-10 |
| **سند مورد نقد** | [KUBEN-GOLDEN-ARCHITECTURE.md](./KUBEN-GOLDEN-ARCHITECTURE.md) v1.0 |
| **معیارهای نقد** | (۱) سادگی در برابر ارزش، (۲) ریسک اجرایی، (۳) قابلیت تحقق با تیم ۲ تا ۴ نفره، (۴) امنیت، (۵) تجربه‌ی واقعی Self-host |

---

## فهرست

- [۰. حکم کلی و نمره](#۰-حکم-کلی-و-نمره)
- [۱. نقدهای ساختاری (خود پلن)](#۱-نقدهای-ساختاری-خود-پلن)
- [۲. نقدهای معماری — نقطه به نقطه](#۲-نقدهای-معماری--نقطه-به-نقطه)
- [۳. ریسک‌هایی که v1.0 ندید یا دست‌کم گرفت](#۳-ریسکهایی-که-v10-ندید-یا-دستکم-گرفت)
- [۴. ساختار Monorepo: نقد و اصلاح](#۴-ساختار-monorepo-نقد-و-اصلاح)
- [۵. پلن نهایی اصلاح‌شده (v1.1)](#۵-پلن-نهایی-اصلاحشده-v11)
- [۶. ADRهای اضافه‌شده](#۶-adrهای-اضافهشده)
- [۷. Definition of Done معماری قبل از اولین خط کد](#۷-definition-of-done-معماری-قبل-از-اولین-خط-کد)
- [۸. جمع‌بندی](#۸-جمعبندی)

---

## ۰. حکم کلی و نمره

**نمره‌ی v1.0: 7.5 از 10.** جهت‌گیری‌ها درست‌اند و تحلیل Kubero مبتنی بر شواهد است، اما سند سه بیماری کلاسیک «معماری روی کاغذ» را دارد:

1. **Gold-plating:** چند مکانیزم پیچیده (دو Runtime، Hash-chain در Audit، سه Transport هم‌زمان، ۱۳ Crate) که ارزش‌شان نسبت به هزینه‌ی پیاده‌سازی و نگهداری اثبات نشده است.
2. **خوش‌بینی زمانی:** جمع فازهای ۰ تا ۳ حدود ۳۰ هفته برای جایگزینی محصولی با ۴۰ هزار خط کد، یک Operator و ۱۶۰ Template. عدد واقعی برای رسیدن به «برابری با Kubero + Enterprise» با تیم ۲ تا ۴ نفره **۱۲ تا ۱۸ ماه** است.
3. **دست‌کم گرفتن سه ریسک عملیاتی:** Registry داخل Cluster (Chicken-and-egg اعتماد Node به TLS)، Bootstrap خود Kuben (Gateway و TLS و Webhook URL قبل از اینکه چیزی نصب باشد)، و Lifecycle خود CRDها (Helm CRDها را Upgrade نمی‌کند).

### جدول تغییرات v1.0 → v1.1

| # | موضوع | v1.0 | v1.1 (اصلاح) | دلیل |
|---|---|---|---|---|
| 1 | دو Tokio Runtime | از فاز ۰ | **یک Runtime** با Discipline سخت‌گیرانه‌ی `spawn_blocking`؛ دو Runtime **پشت Config** و فقط بعد از Benchmark فاز ۱ | پیچیدگی State مشترک، Client دوگانه، Tracing؛ Starvation واقعی با `spawn_blocking` و Semaphore حل می‌شود |
| 2 | REST + SSE + WS | سه Transport | **REST + یک SSE برای هر تب (Scope = Project) + WS فقط برای Terminal**؛ Log هم روی همان SSE تب Multiplex می‌شود | حذف Subscribe API، حذف مشکل ۶ Connection، یک مسیر Reconnect |
| 3 | Audit Hash-chain | سراسری | **حذف Hash-chain از هسته**؛ Append-only + Export امضاشده‌ی دوره‌ای (Merkle Anchor) در Enterprise | Chain سراسری در Postgres چند-Replica یک Serialization Point است |
| 4 | Session Cache | moka با TTL ۶۰ ثانیه | TTL **۵ ثانیه** + `sessions.revoked_at` + Version Counter برای Replicaها | ادعای «Revoke فوری» با TTL ۶۰ ثانیه غلط بود |
| 5 | Build | Job با Init Container برای Fetch + BuildKit | **buildkitd پایدار (Rootless StatefulSet + Cache PVC)** + Job سبک `buildctl`؛ Fetch توسط خود BuildKit (Git Context)؛ Railpack به‌عنوان BuildKit Frontend؛ CNB به فاز ۳ | Cache پایدار = Buildهای ۱۰ برابر سریع‌تر؛ حذف Fetch Image اختصاصی |
| 6 | Registry داخلی | Zot «اختیاری» | **Registry خارجی در فاز ۱ الزامی**؛ Zot داخلی در فاز ۲ فقط با دامنه‌ی عمومی + گواهی معتبر (ACME) | Node باید به Registry اعتماد کند؛ Self-signed در Kubernetes یک کابوس عملیاتی است |
| 7 | Build Logs | zstd در SQL | **فایل روی PVC یا `object_store`** با Retention؛ در SQL فقط Metadata | SQLite با Blobهای ۵MB بزرگ و کند می‌شود |
| 8 | Metrics Lite | متکی به metrics-server | metrics-server **اگر بود**؛ در غیر این صورت `kubelet /stats/summary` یا نمایش «Metrics unavailable» با راهنمای نصب | metrics-server روی همه‌ی Clusterها پیش‌فرض نیست |
| 9 | Release Snapshot | «Snapshot پیکربندی» | Snapshot از **Reference + resourceVersion** Secretها، نه مقدار | مقدار Secret هرگز در CR قرار نمی‌گیرد |
| 10 | Crateها | ۱۳ Crate از روز اول | **۶ Crate** در فاز ۰؛ Split فقط وقتی Compile Time یا مرز Team ایجاب کند | Over-splitting زودهنگام = Friction بی‌دلیل |
| 11 | Roadmap | ~۳۰ هفته تا Enterprise | **MVP ۱۲ هفته‌ای** با Scope بسته + ۱۲ تا ۱۸ ماه تا فاز ۳ | واقع‌گرایی |
| 12 | CRD Lifecycle | فقط سیاست Versioning | **Kuben خودش CRDها را در Boot Apply می‌کند** (Server-Side Apply) + Ratcheting + Storage Version Migration | Helm فایل‌های `crds/` را Upgrade نمی‌کند |
| 13 | Bootstrap | نانوشته | **بخش Bootstrap صریح**: دامنه‌ی پیش‌فرض `sslip.io`، Port-forward اول، ترتیب نصب Gateway → cert-manager → Kuben | Chicken-and-egg |
| 14 | حذف Environment | Finalizer | **Soft-delete با Grace Period ۷ روزه** + Annotation `protected` + تایپ نام برای تأیید | Blast Radius حذف Namespace |
| 15 | RBAC خود Kuben | «Least Privilege» | صادقانه: Kuben یک **Cluster-admin-lite** است؛ در HA، Role `api` با RBAC محدودتر از `controller` | امنیت واقعی نه شعار |
| 16 | SQLite → Postgres | نانوشته | دستور `kuben db migrate --to postgres` از فاز ۲ | مسیر رشد Solo → HA |

---

## ۱. نقدهای ساختاری (خود پلن)

### ۱.۱ «همه‌چیز D1» یعنی هیچ‌چیز D1 نیست

در بخش ۸ سند v1.0، بیش از ۴۰ مورد برچسب D1 (روز اول) دارند. وقتی همه‌چیز اولویت اول است، تیم عملاً بدون اولویت کار می‌کند و فاز ۰ به یک باتلاق ۴ ماهه تبدیل می‌شود.

**اصلاح:** سه سطح جدید:

- **D1-Structural:** چیزهایی که بعداً اضافه کردن‌شان به بازنویسی می‌انجامد (مرز داده، مدل Session، Bounded بودن Streamها، SSA، Idempotency در Deploy، Projection). این‌ها **باید** در فاز ۰ باشند.
- **D1-Stub:** Interface در فاز ۰، پیاده‌سازی ساده (Leader Election با یک Trait و پیاده‌سازی `NoopLeader`؛ Circuit Breaker با یک Trait؛ OIDC فقط Trait `IdentityProvider` + پیاده‌سازی Local).
- **D2:** بقیه.

فهرست دقیق در بخش ۵.

### ۱.۲ زمان‌بندی خوش‌بینانه است

| فاز | v1.0 | برآورد واقع‌بینانه (۳ نفر Rust-fluent) | دلیل اختلاف |
|---|---|---|---|
| ۰ | ۴ تا ۶ هفته | ۸ تا ۱۰ هفته | Auth کامل + دو DB + CRD + OpenAPI Pipeline + UI Shell + CI Budgetها = ۶ Track موازی |
| ۱ | ۸ هفته | ۱۲ تا ۱۶ هفته | Build System همیشه ۲ برابر برآورد طول می‌کشد؛ LogHub و Terminal هر کدام ۲ هفته با تست Soak |
| ۲ | ۸ هفته | ۱۶ تا ۲۰ هفته | ۵ Git Provider، ۳ Data Service، Importer، Template Catalog، CLI، Helm |
| ۳ | ۸ تا ۱۰ هفته | ۱۲ تا ۱۶ هفته | HA و Multi-cluster نیازمند Chaos Testing واقعی |
| **جمع** | **~۳۰ هفته** | **~۵۰ تا ۶۰ هفته** | |

**اصلاح:** به‌جای کشیدن زمان فازها، **Scope را ببرید.** بخش ۵ یک «MVP واقعی ۱۲ هفته‌ای» تعریف می‌کند که یک کاربر واقعی بتواند با آن زندگی کند.

### ۱.۳ سند، Tenetها را در چند جا نقض می‌کند

- Tenet ۷ می‌گوید «Zero-Ops با Escape Hatch»، ولی طرح Zot داخلی + cert-manager + Gateway + metrics-server + BuildKit، پنج وابستگی خارجی را در نصب پایه می‌آورد. Zero-Ops یعنی Installer **همه‌ی این‌ها را** با یک دستور و با پیش‌فرض‌های درست نصب و **Health آن‌ها را در UI** نشان دهد. این کار در هیچ فازی برآورد نشده بود.
- Tenet ۳ (همه‌چیز Bounded) در بخش Release رعایت نشده: «Releaseهای N تای آخر در etcd» ولی N تعریف نشده و Garbage Collection آن در هیچ Reconciler‌ای نیست.

### ۱.۴ چیزهایی که باید در سند بودند ولی نبودند

- **مدل تست Controllerها** بدون Cluster (Fake API Server با `tower-test` / `hyper` Mock در kube-rs) — بدون این، هر تست Controller نیازمند kind است و CI کند می‌شود.
- **استراتژی Observability برای خود Development** (چطور یک Reconcile خراب را Debug کنیم؟ `kuben debug reconcile app/foo --dry-run` که Manifestهای خروجی را چاپ کند).
- **مدل نسخه‌بندی و سازگاری** بین CLI، API، CRD و Helm Chart.
- **Threat Model** رسمی (STRIDE مختصر) — سند امنیت را در Checklist دید، نه در Threat Model.

---

## ۲. نقدهای معماری — نقطه به نقطه

هر مورد: **ادعای v1.0 → مشکل → اصلاح.**

### ۲.۱ دو Tokio Runtime (Bulkhead) — پیچیدگی زودهنگام

**ادعا:** دو Runtime ایزوله‌سازی CPU می‌دهد و ارزان است.

**مشکل:**
- «ارزان» فقط از نظر Thread است. هزینه‌ی واقعی: هر Type مشترک باید `Send + Sync + 'static` باشد و از مرز Runtime عبور کند؛ `tracing` Span Context بین Runtimeها منتقل نمی‌شود؛ دو `kube::Client`، دو Connection Pool، دو مجموعه Metric؛ Testها باید هر دو را بالا بیاورند؛ `block_on` تو در تو یک Panic کلاسیک است.
- Starvationی که سند خودش لیست کرد (Argon2، Deserialize بزرگ، YAML) همگی با `spawn_blocking` + Semaphore حل می‌شوند، **نه** با دو Runtime. دو Runtime فقط از «کد CPU-bound که فراموش کردیم `spawn_blocking` کنیم» محافظت می‌کند؛ ولی همان کد در Runtime دوم هم Reconcile را می‌کشد.
- ایزوله‌سازی حافظه اصلاً به دست نمی‌آید (خود سند اعتراف کرده).

**اصلاح (v1.1):**
- فاز ۰: **یک Runtime** (`worker_threads = min(4, cpus)`، `max_blocking_threads = 16`).
- Discipline قابل Lint: Clippy `await_holding_lock`، یک Lint سفارشی (یا Review Rule) که هر تابع با نام `*_blocking` یا Crateهای شناخته‌شده‌ی CPU-bound (argon2، serde_yaml روی ورودی بزرگ، zstd) فقط داخل `spawn_blocking` صدا زده شود.
- `tokio::task::Builder` با نام برای هر Task (Feature `tracing`) تا `tokio-console` مقصر Starvation را نشان دهد.
- **Abstraction برای آینده:** یک Struct `Runtimes { api: Handle, ctrl: Handle }` که در فاز ۰ هر دو Handle به یک Runtime اشاره می‌کنند. اگر Benchmark فاز ۱ نشان داد Tail Latency زیر بار Reconcile خراب می‌شود، سوئیچ به دو Runtime یک تغییر ۵۰ خطی در `main.rs` است.

### ۲.۲ مرز داده: v1.0 یک دوپارگی جدید ساخت

**ادعا:** «CRD = Desired State، SQL = Identity.» تمیز.

**مشکل:** `Project` و `Environment` هم CRD هستند و هم در `role_bindings.scope_id` در SQL ارجاع می‌شوند. وقتی کسی با `kubectl delete project foo` CR را حذف کند، Bindingهای SQL یتیم می‌مانند. وقتی SQL Restore شود ولی etcd نه (یا برعکس)، ارجاع‌ها بی‌معنی می‌شوند. **همان دوپارگی Kubero، فقط با اسم‌های تمیزتر.**

**اصلاح:**
1. **قانون مالکیت:** هر موجودیت دقیقاً یک مالک دارد. Org، User، Membership، Session، Token، Audit → SQL. Project، Environment، App، Release، BuildRun، Domain، Service → CRD.
2. **ارجاع SQL به CRD فقط با `uid`** (نه نام)، و یک **Reconciler پاکسازی** (`BindingGC`) که Bindingهای با `scope_uid` ناموجود را با تأخیر ۱ ساعته حذف می‌کند (نه فوری، تا Restore جزئی داده را نابود نکند).
3. **Org روی CRD با Label:** `kuben.dev/org: <org-id>` روی Project. Authorization = (RoleBinding در SQL) ∩ (Label روی CR). Cache در حافظه.
4. **Backup اتمیک:** دستور `kuben backup` هر دو را با هم و با یک Manifest مشترک (Timestamp + Checksum) ذخیره می‌کند، و `restore` هر دو را با هم برمی‌گرداند یا هیچ‌کدام.

### ۲.۳ سه Transport هم‌زمان (REST + SSE + WS)

**ادعا:** SSE برای رویدادها و لاگ، WS برای ترمینال، و «یک SSE Multiplexed برای هر تب با Subscribe API».

**مشکل:** «Subscribe API» یعنی یک POST جانبی که Server باید به Connection SSE ربط دهد (Session Registry، Race بین Subscribe و اولین Event، Cleanup وقتی SSE قطع شد). این چیزی است که WebSocket به‌صورت Native می‌دهد. سند هم WS را دارد، هم SSE را، هم یک Subscribe API؛ سه مکانیزم Reconnect متفاوت در Client.

**اصلاح (ساده‌تر و بدون افت قابلیت):**
- **یک SSE برای هر تب با Scope در URL:** `GET /api/v1/stream?project=<uid>&logs=<app>:<pod>:<container>,...`. تغییر Scope (Navigation یا باز کردن Log Viewer) = بستن و باز کردن SSE با URL جدید. چون مدل Snapshot + Delta است و `Last-Event-ID` داریم، Reopen تقریباً رایگان است. Server فقط Filter بر اساس Permission می‌کند.
- **Log Viewerهای هم‌زمان** روی همان SSE با Event Type `log` و فیلد `stream_id` Multiplex می‌شوند. LogHub بدون تغییر می‌ماند.
- **WS فقط برای Terminal** (باینری، Backpressure).
- نتیجه: **حداکثر ۱ SSE + N Terminal WS برای هر تب.** مشکل ۶ Connection عملاً حل می‌شود حتی بدون HTTP/2.
- **Rule سخت:** هیچ Endpoint دیگری Stream نمی‌کند. اگر کسی Long-poll یا SSE جدید خواست، باید ADR بنویسد.

### ۲.۴ Session Cache و ادعای «Revoke فوری»

**ادعا:** Session در DB، Cache با moka TTL ۶۰ ثانیه، «Revoke فوری».

**مشکل:** با TTL ۶۰ ثانیه، Session حذف‌شده تا ۶۰ ثانیه معتبر می‌ماند؛ در حالت HA هر Replica Cache خودش را دارد. این «فوری» نیست، و برای سناریوی «کارمند اخراج‌شده» یا «Token لو رفته» مهم است.

**اصلاح:**
- TTL Cache **۵ ثانیه** (هزینه‌ی DB: یک Point Lookup روی Primary Key هر ۵ ثانیه برای هر کاربر فعال — ناچیز).
- عملیات حساس (`app:exec`، `secret:read`، تغییر RBAC، حذف) **همیشه** Session را از DB می‌خوانند (Bypass Cache).
- در HA: جدول `revocations (subject_hash, at)` + هر Replica هر ۲ ثانیه `MAX(at)` را می‌خواند و در صورت تغییر Cache را Invalidate می‌کند. ساده‌تر از Pub/Sub و بدون Redis.
- **Session Binding سبک:** ذخیره‌ی `ua_hash` و `/24` IP در Session؛ تغییر هر دو با هم → الزام Re-auth. (نه Binding سخت به IP؛ کاربران Mobile را می‌شکند.)

### ۲.۵ Audit Hash-chain

**ادعا:** Audit با `prev_hash`/`hash` برای Tamper-evidence.

**مشکل:**
- Chain سراسری = هر Insert باید آخرین Hash را بخواند → Serialization کامل نوشتن Audit در Postgres چند-Replica (Advisory Lock یا Retry Loop). دقیقاً جایی که HA لازم است، Bottleneck می‌سازد.
- Tamper-evidence در برابر کسی که به DB دسترسی دارد بی‌معنی است، چون همان شخص می‌تواند Chain را از نقطه‌ی دلخواه بازسازی کند. ارزش واقعی فقط با **Anchor خارجی** به دست می‌آید.

**اصلاح:**
- هسته: Audit **Append-only** (بدون UPDATE/DELETE از طریق Role DB محدود) + `seq` یکنواخت + Export.
- Enterprise: **Merkle Anchor دوره‌ای** — هر ۵ دقیقه Root Hash از Batch جدید محاسبه، با کلید Ed25519 امضا و به Object Storage خارجی یا Syslog/SIEM ارسال می‌شود. بدون Serialization در Write Path.

### ۲.۶ Build System — بزرگ‌ترین بازنگری

**ادعا:** برای هر Build یک Job با Init Container `fetch` + BuildKit؛ Kaniko نه؛ Railpack و CNB.

**مشکل‌ها:**
1. **بدون Cache پایدار، Buildها کند می‌مانند.** Job-per-build با Registry Cache کمک می‌کند ولی Layer Cache روی دیسک ۵ تا ۱۰ برابر سریع‌تر است. کاربران Coolify/Dokploy به Buildهای Warm ۲۰ ثانیه‌ای عادت دارند.
2. Init Container `fetch` اختصاصی (مثل `ghcr.io/kubero-dev/fetch`) یک Image دیگر برای نگهداری است. **BuildKit خودش Git Context را با Auth (Secret) می‌فهمد.**
3. CNB نیازمند Lifecycle Image، Builder Image و Platform API است — یک زیرسیستم کامل. در فاز ۱ جایی ندارد.
4. BuildKit Rootless در Kubernetes به `securityContext` خاص (`seccompProfile: Unconfined`، `apparmor: unconfined`، و گاهی `procMount`) نیاز دارد که با PSA `restricted` جمع نمی‌شود. سند این را یک خط گفته بود ولی عملیاتی نکرده بود.

**اصلاح (v1.1):**
```
kuben-builds namespace  (PSA: baseline، نه restricted — صریح و مستند)
├── buildkitd            StatefulSet, 1 replica, rootless, PVC cache (پیش‌فرض 20Gi، GC داخلی BuildKit)
│                        mTLS با گواهی تولیدشده توسط Kuben (نه cert-manager؛ داخلی)
└── build-<app>-<sha>    Job، image: buildctl (pinned digest)، بدون SA token، NetworkPolicy: فقط buildkitd + registry + git
                         buildctl build --addr tcp://buildkitd:1234
                           --frontend dockerfile.v0 | --frontend gateway.v0 --opt source=<railpack-frontend@digest>
                           --opt context=git://... (secret: git token)
                           --export-cache type=inline
                           --output type=image,name=<registry>/<app>:<sha>,push=true
                         → termination message: image digest
```
- Controller: Job Completion را Watch می‌کند، Digest را از `terminationMessage` (یا Registry HEAD) می‌خواند و `Release` می‌سازد. Build Pod هیچ‌وقت API Server را نمی‌بیند (Invariant ۵ حفظ می‌شود).
- **استراتژی‌ها در فاز ۱:** `dockerfile` و `railpack` (هر دو BuildKit Frontend، یک مسیر کد). `image` (بدون Build). CNB → فاز ۳ با `kpack` یا `pack` به‌عنوان Integration، نه پیاده‌سازی داخلی.
- **Queue:** `BuildRun` CR + Concurrency Limit در Controller (`max_concurrent_builds` سراسری و برای هر Org). buildkitd خودش Parallel Build را مدیریت می‌کند.
- **Scale:** برای Enterprise، buildkitd به N Replica با `buildctl --addr` Round-robin، یا Node Pool جدا.
- **Fallback بدون Privilege:** اگر Cluster اجازه‌ی Rootless BuildKit نداد (بعضی Managed Clusterها با Policy سخت)، استراتژی `image` + **Remote Build** (GitHub Actions Template که Image را Push و Webhook می‌زند). این Fallback باید در فاز ۲ مستند و آماده باشد.

### ۲.۷ Registry داخلی — Chicken-and-egg اعتماد Node

**ادعا:** Zot داخلی «اختیاری برای نصب‌های Solo».

**مشکل:** kubelet هر Node باید بتواند از Registry Pull کند. Registry داخل Cluster با Self-signed TLS = باید روی **هر Node** به `containerd` گفته شود این CA را قبول کند (فایل `/etc/rancher/k3s/registries.yaml` یا `/etc/containerd/certs.d/`). این یعنی SSH به Nodeها — دقیقاً ضد Zero-Ops، و روی Managed Clusterها غیرممکن. Insecure HTTP هم همین مشکل را دارد. این ریسک در v1.0 یک خط بود؛ در واقعیت **رایج‌ترین دلیل شکست نصب PaaSهای Kubernetes** است.

**اصلاح:**
- **فاز ۱:** Registry خارجی **الزامی** (ghcr.io، Docker Hub، GitLab Registry، ECR/GCR/ACR، یا هر OCI Registry). Wizard نصب Credential می‌گیرد و با یک Push آزمایشی اعتبارسنجی می‌کند. این همان کاری است که Kubero هم عملاً انتظار دارد.
- **فاز ۲:** Zot داخلی **فقط** وقتی که (الف) Gateway + cert-manager با ACME کار می‌کنند و (ب) دامنه‌ی عمومی (`registry.<base-domain>`) دارد. Nodeها به Let's Encrypt اعتماد دارند → بدون دست زدن به Node. Pull از داخل Cluster از طریق همان دامنه‌ی عمومی (Hairpin) انجام می‌شود؛ Installer باید بررسی کند که Hairpin NAT کار می‌کند.
- **k3s Path:** روی k3s، Installer می‌تواند `--embedded-registry` (Spegel) را فعال کند تا Pull بین Nodeها Cache شود؛ ولی این جایگزین Registry نیست.
- **UI:** وضعیت Registry (Reachability از Node) به‌عنوان یک Health Check دائمی نمایش داده شود، با پیام خطای قابل‌فهم.

### ۲.۸ Build Logs در SQLite

**مشکل:** Blobهای چند مگابایتی داخل SQLite: فایل DB بزرگ می‌شود، `VACUUM INTO` برای Backup کند می‌شود، و Page Cache از داده‌ی مهم (Session، RBAC) پر می‌شود.

**اصلاح:** Log هر Build به‌صورت فایل `zstd` روی PVC خود Kuben (`/data/build-logs/<app>/<sha>.log.zst`) یا `object_store` (اگر S3 پیکربندی شده). در SQL فقط `(build_id, path, size, sha256)`. Retention: ۵۰ Build آخر برای هر App یا ۳۰ روز، هر کدام زودتر. Streaming حین Build مستقیماً از Pod Log Stream (LogHub).

### ۲.۹ Metrics Lite و metrics-server

**مشکل:** metrics-server روی k3s هست، روی EKS/GKE/AKS پیش‌فرض نیست (یا با تنظیمات متفاوت). سند «بدون Prometheus» را وعده داد ولی به یک وابستگی دیگر تکیه کرد.

**اصلاح:**
- تشخیص خودکار: اگر `metrics.k8s.io` موجود است → Poll هر ۱۵ ثانیه (یک List برای کل Cluster با Label Selector).
- اگر نیست: Installer آن را نصب می‌کند (Helm Chart کوچک، Zero-config)؛ روی Managed Clusterها فقط راهنمای یک‌خطی.
