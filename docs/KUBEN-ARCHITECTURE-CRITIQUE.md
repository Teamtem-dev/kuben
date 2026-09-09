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
- Fallback **بدون هیچ وابستگی:** `GET /api/v1/nodes/<node>/proxy/stats/summary` (kubelet Summary API از طریق API Server Proxy). نیازمند RBAC `nodes/proxy` است و کمی سنگین‌تر، ولی همه‌جا کار می‌کند. در فاز ۲.
- UI هرگز Crash یا Placeholder خالی نشان نمی‌دهد؛ «Metrics unavailable — Install metrics-server» با دکمه.

### ۲.۱۰ Release و Secretها

**مشکل:** «Release = Digest + Snapshot پیکربندی» اگر لفظاً پیاده شود، مقادیر Env (که شامل Secret‌اند) وارد یک CR می‌شوند که با `get releases` خوانده می‌شود.

**اصلاح:** Release شامل: Digest، Spec غیرحساس App (Processes، Scaling، Domains)، و برای هر Secret فقط `{name, resourceVersion}`. Rollback = بازگرداندن Spec + بررسی اینکه Secretها هنوز وجود دارند (اگر تغییر کرده‌اند، UI هشدار «Secrets changed since this release» می‌دهد). Secretهای App خودشان Immutable-versioned می‌شوند: `app-<name>-env-<hash>` با `immutable: true` و GC برای نسخه‌های بدون ارجاع. این هم Rollback دقیق می‌دهد و هم Rolling Update خودکار هنگام تغییر Env (چون نام Secret در Pod Template عوض می‌شود).

### ۲.۱۱ Terminal — دو جزئیات فراموش‌شده

1. **کانال کنترل هنگام Pause:** در کد نمونه، وقتی `paused` است `stdout.read` اجرا نمی‌شود، ولی Message Resize/Control همچنان از Browser می‌رسد و Handle می‌شود — درست. اما **`exit` پروسه هنگام Pause** تشخیص داده نمی‌شود تا Resume شود. اصلاح: `proc.join()` را به‌عنوان یک شاخه‌ی `select!` جدا اضافه کنید.
2. **Ephemeral Debug Container:** بعد از ساخته شدن **قابل حذف نیست** تا Pod Restart شود، و Process Namespace اشتراکی فقط با `shareProcessNamespace` یا `targetContainerName` کار می‌کند. UI باید این را بگوید («Debug container stays until pod restart») و Audit ثبت کند. نیازمند RBAC `pods/ephemeralcontainers`.

### ۲.۱۲ mimalloc و بودجه‌ی RSS

**مشکل:** mimalloc حافظه‌ی آزادشده را با تأخیر به OS برمی‌گرداند (Purge Delay) و Segmentهای ۴MiB رزرو می‌کند؛ RSS Idle ممکن است ۵ تا ۱۰MiB بالاتر از Allocator سیستمی باشد — دقیقاً چیزی که با بودجه‌ی ۲۵MiB می‌جنگد.

**اصلاح:** Benchmark سه گزینه در CI فاز ۰ (musl default، mimalloc با `MIMALLOC_PURGE_DELAY=0`/`mi_option_purge_delay`، jemalloc با `background_thread` و `dirty_decay_ms` کم) و **انتخاب با عدد**. حدس اولیه: mimalloc با Purge کوتاه.

### ۲.۱۳ Frontend — ریزه‌کاری‌های عملی

- **401 سراسری:** یک `QueryClient` Interceptor که 401 را به «Session expired» Modal تبدیل و بعد از Login مجدد Queryها را Resume کند، نه Redirect خام. (در Kubero بعد از Expire شدن JWT، UI بی‌صدا خالی می‌شود.)
- **Optimistic Concurrency:** فرم App باید `resourceVersion` را با `If-Match` بفرستد؛ Conflict → Diff نشان بده، نه Overwrite.
- **RTL و i18n در فاز ۰:** Logical Properties از اول (`ms-`، `pe-`، `start`/`end`) چون بعداً Retrofit کردن ۳۰۰ کامپوننت عذاب است؛ ولی **ترجمه‌ی واقعی** (فارسی، آلمانی، …) فاز ۲ — فقط زیرساخت `paraglide` در فاز ۰.
- **بودجه‌ی Bundle:** xterm + WebGL addon + uPlot + CodeMirror با هم حدود ۱۵۰ تا ۲۰۰KB Brotli هستند؛ بودجه‌ی «۲۰۰KB برای Shell اولیه» فقط با Lazy Route برای Terminal، Logs و Editor نگه داشته می‌شود. این باید در `size-limit` به تفکیک Route اعمال شود، نه کل.

---

## ۳. ریسک‌هایی که v1.0 ندید یا دست‌کم گرفت

### ۳.۱ Bootstrap خود Kuben (Chicken-and-egg)

Kuben برای دریافت Webhook از GitHub به URL عمومی نیاز دارد؛ URL عمومی به Gateway + DNS + TLS نیاز دارد؛ Gateway را خود Kuben باید نصب کند. در v1.0 هیچ بخشی این ترتیب را تعریف نکرده بود.

**اصلاح — ترتیب Bootstrap رسمی:**
1. `install.sh`: (k3s در صورت نیاز) → Gateway Implementation (Traefik یا Envoy Gateway) → cert-manager → Kuben (Helm/Manifest).
2. Kuben در اولین Boot: CRDها را Apply می‌کند، Admin را می‌سازد (Password یک‌بارمصرف در Log و در یک Secret)، و **بدون دامنه** قابل استفاده است: `kubectl port-forward` یا NodePort، یا دامنه‌ی خودکار `<ip>.sslip.io` با گواهی ACME HTTP-01 (که با IP عمومی کار می‌کند).
3. Wizard اولیه: Base Domain (یا ادامه با sslip.io)، Registry، Git Provider. هر مرحله Test دارد.
4. Webhookها تا وقتی دامنه‌ی عمومی نیست، به **Polling Fallback** (هر ۶۰ ثانیه `ls-remote`) می‌افتند — بدون اینکه کاربر بفهمد. این همان Catch-up مکانیزم بخش ۵.۵ v1.0 است، فقط دائمی.
5. `kuben doctor`: CLI و صفحه‌ی UI که تمام Preflightها را چک می‌کند (DNS، Hairpin، Registry، PSA، StorageClass RWO، metrics-server، Gateway Class).

### ۳.۲ Lifecycle خود CRDها

- Helm پوشه‌ی `crds/` را فقط در Install می‌گذارد و در Upgrade دست نمی‌زند. **Kuben باید در Boot، CRDهای Embedded خودش را با Server-Side Apply اعمال کند** (نیازمند RBAC روی `customresourcedefinitions`؛ در حالت HA فقط Leader). اگر RBAC نبود: Log خطای واضح + Helm Hook Job به‌عنوان جایگزین.
- **CRD Validation Ratcheting** (K8s ≥1.30): اضافه کردن CEL Rule جدید نباید Objectهای موجود را Invalid کند برای Updateهایی که آن فیلد را عوض نمی‌کنند. Ratcheting پیش‌فرض فعال است ولی باید تست شود.
- **Storage Version Migration:** وقتی `v1beta1` اضافه می‌شود، همه‌ی Objectهای ذخیره‌شده با `v1alpha1` باید Rewrite شوند (`kube-storage-version-migrator` یا یک Job داخلی که همه را Touch می‌کند). این را در ADR-CRD صریح کنید.
- **Downgrade:** Kuben نسخه‌ی N-1 با CRD نسخه‌ی N: باید فیلدهای ناشناخته را حفظ کند (`x-kubernetes-preserve-unknown-fields` روی Spec سطح بالا، و `#[serde(flatten)] extra: BTreeMap`).

### ۳.۳ RBAC خود Kuben = Cluster-admin-lite

Kuben Namespace می‌سازد، در همه‌ی Namespaceها Secret و RoleBinding و NetworkPolicy می‌نویسد، Pod Exec می‌کند و CRD Apply می‌کند. **هر RCE در Kuben = تصاحب کامل Cluster.** v1.0 نوشت «Least Privilege» که در این جایگاه شعار است.

**اصلاح صادقانه:**
- در مستندات: «Kuben admin عملاً cluster-admin است. Kuben را در Namespace اختصاصی با NetworkPolicy سخت نصب کنید.»
- **در حالت HA:** Role `api` فقط RBAC خواندن + نوشتن CRهای Kuben + `pods/log` + `pods/exec` را دارد؛ Role `controller` RBAC گسترده. یک RCE در API فقط CR می‌نویسد (که Controller Validate می‌کند)، نه Secret و RoleBinding.
- **در حالت `all`:** این جداسازی وجود ندارد — صریحاً مستند شود.
- Impersonation (Enterprise): API با `Impersonate-User` کار می‌کند تا Audit خود K8s کاربر واقعی را ببیند و RBAC K8s لایه‌ی دوم دفاع باشد.

### ۳.۴ Blast Radius حذف Environment

حذف Environment = حذف Namespace = حذف همه‌ی Podها، PVCها و Secretها. یک کلیک اشتباه یا یک باگ در Finalizer، داده‌ی Production را نابود می‌کند.

**اصلاح:**
- `Environment` با `spec.deletionPolicy: Retain | Delete` (پیش‌فرض `Retain` برای `production` و `Delete` برای `preview`).
- **Soft-delete:** UI حذف را به `status.phase: Terminating` + `deletionScheduledAt = now + 7d` تبدیل می‌کند؛ Workloadها به ۰ Scale می‌شوند (Cost صفر)، PVCها می‌مانند؛ Restore با یک کلیک تا ۷ روز. Reconciler بعد از مهلت، حذف واقعی را انجام می‌دهد.
- Annotation `kuben.dev/protected: "true"` روی Environment و App → حذف فقط با تایپ نام + Permission `env:delete-protected`.
- **هرگز** Namespace را که Kuben نساخته (Adopted از Kubero) حذف نکن مگر با Flag صریح.

### ۳.۵ مسیر Solo → HA

کاربری که با SQLite شروع کرده و رشد کرده، باید بتواند به Postgres برود بدون اینکه از نو نصب کند. v1.0 هیچ ابزاری برای این نداشت.

**اصلاح:** `kuben db migrate --from sqlite:///data/kuben.db --to postgres://...` (Copy جدول به جدول با Transaction، Verify با Row Count و Checksum، و Cutover با تغییر `DATABASE_URL`). فاز ۲.

### ۳.۶ رمزنگاری etcd

Secretها در etcd به‌صورت پیش‌فرض رمزنگاری نمی‌شوند. Installer k3s باید `--secrets-encryption` را فعال کند؛ `kuben doctor` روی Clusterهای دیگر هشدار بدهد.

### ۳.۷ Threat Model حداقلی (که v1.0 نداشت)

| تهدید | مهاجم | کنترل |
|---|---|---|
| Tenant A لاگ/ترمینال Tenant B را می‌بیند | کاربر احراز هویت‌شده | Authorization در هر Subscription (Invariant ۲)؛ تست E2E منفی الزامی |
| کد Build کاربر به Cluster حمله می‌کند | Developer بدخواه یا Dependency آلوده | Namespace ایزوله، بدون SA Token، NetworkPolicy Egress محدود، Node Pool جدا در Enterprise |
| App کاربر به API Server یا Kuben می‌رسد | Container Compromised | `automountServiceAccountToken: false`، NetworkPolicy Default-deny به `kube-system` و Namespace Kuben |
| سرقت Session مرورگر | XSS، Malicious Extension | Cookie HttpOnly، CSP سخت، TTL کوتاه Cache، Re-auth برای عملیات حساس |
| Webhook جعلی Deploy می‌کند | مهاجم شبکه | HMAC + Delivery-ID Dedupe + فقط Branchهای پیکربندی‌شده |
| Supply Chain (Image کمکی آلوده) | Upstream | همه‌ی Imageها Digest-pinned، Renovate با Review، cosign Verify برای Imageهای Kuben |
| RCE در Kuben | هر کس | بخش ۳.۳؛ Kuben در Namespace خودش با PSA `restricted` و ReadOnlyRootFilesystem |
| Admin بدخواه | Insider | Audit Append-only + Export خارجی؛ 4-eyes برای Production (Enterprise) |

---

## ۴. ساختار Monorepo: نقد و اصلاح

### ۴.۱ ۱۳ Crate زیاد است

Over-splitting زودهنگام: هر مرز Crate یعنی `pub` کردن Typeها، Feature Flagهای عبوری، و Compile Unitهایی که به‌هرحال با هم Rebuild می‌شوند. Split وقتی ارزش دارد که (الف) Compile Time یک Crate بزرگ آزاردهنده شود، (ب) یک Crate مستقلاً منتشر شود (`kuben-crd` برای Toolهای شخص ثالث، `kuben-client` برای CLI)، یا (ج) مرز Team باشد.

**ساختار v1.1 برای فاز ۰:**
```
crates/
├── kuben-crd/        # CRD types + crdgen  (منتشرشدنی؛ بدون tokio/axum)
├── kuben-core/       # domain، errors، config، ids، policy trait، store trait  (بدون IO)
├── kuben-store/      # sqlx + sea-query + migrations
├── kuben-platform/   # k8s (registry، informers، projections، loghub، exec) + controllers + build
├── kuben-api/        # axum + openapi + sse/ws + web assets embed + bin: openapi
└── kuben/            # bin: server (roles، supervisor، signals) + cli subcommands
```
- `kuben-auth` داخل `kuben-core` (Traits) و `kuben-api` (پیاده‌سازی HTTP) می‌رود.
- `kuben-controller`، `kuben-k8s`، `kuben-build` با هم در `kuben-platform`؛ وقتی به ۱۵ هزار خط رسید Split شود.
- `kuben-telemetry` یک Module در `kuben`.
- `kuben-cli` جدا **فقط وقتی** CLI مستقل منتشر می‌شود (فاز ۲) — آن موقع `kuben-client` (تولیدشده از OpenAPI با `progenitor` یا دستی) هم اضافه می‌شود.
- `kuben-testkit` → `dev-dependencies` مشترک در `kuben-core` با Feature `test-util`.

### ۴.۲ Template Catalog

Repo جدا از روز اول (`kuben-templates`)، با لایسنس مستقل، CI اعتبارسنجی Schema، و Kuben آن را به‌عنوان OCI Artifact یا Tarball نسخه‌دار می‌کشد (Air-gap: Bundle داخل Image یا Mirror). Kubero Templateها را در Repo اصلی دارد که Release و License را گره می‌زند.

### ۴.۳ `apps/docs` با Astro

نه در فاز ۰. یک `README` خوب + `docs/` Markdown کافی است تا Beta. Astro Starlight در فاز ۲ همراه با CLI.

### ۴.۴ Generated Files در Git

`packages/api-client/schema.d.ts` و `charts/kuben/crds/*.yaml` Commit می‌شوند (تا Frontend بدون Rust Toolchain Build شود) و CI با `git diff --exit-code` Drift را می‌گیرد — درست بود. اضافه: یک `CODEOWNERS` که تغییر این فایل‌ها بدون تغییر Source را Block کند (Review Rule).

---

## ۵. پلن نهایی اصلاح‌شده (v1.1)

### ۵.۱ MVP واقعی ۱۲ هفته‌ای («یک Developer با آن زندگی کند»)

Scope بسته — هر چیزی خارج از این لیست تا پایان MVP **نه**:

| حوزه | داخل MVP | بیرون MVP |
|---|---|---|
| Auth | Local User + Password (Argon2id)، Cookie Session، یک Org، Roleهای `admin`/`developer`/`viewer` | OIDC، Passkeys، TOTP، Multi-org، Scoped Bindings |
| مدل | Project، Environment (یک Namespace)، App، Release، BuildRun | Domain CRD (دامنه به‌عنوان فیلد App)، Service/Addon، Backup |
| Deploy | از Image (Registry خارجی) + از Git با Dockerfile/Railpack روی buildkitd؛ Rollback به Release قبلی | CNB، Review Apps، Promotion، Cron |
| Networking | HTTPRoute روی Gateway نصب‌شده توسط Installer + TLS با cert-manager؛ دامنه‌ی `*.sslip.io` پیش‌فرض | Custom Domain Verification، Basic Auth |
| Realtime | SSE برای هر تب (Status + Logs)، Terminal WS | Metrics (فقط Replicas/Status)، Events K8s در UI |
| Git | GitHub Webhook + Polling Fallback | GitLab، Gitea، Bitbucket |
| Ops | `install.sh` برای k3s، `kuben doctor`، `backup`/`restore` SQLite + CRD Export | HA، Multi-cluster، Notifications |
| UI | Login، Projects، App Detail (Overview، Deploys، Logs، Terminal، Settings)، Command Palette | Dark/Light Toggle (فقط Dark)، i18n (فقط EN، ولی RTL-ready) |

**Exit MVP:** یک نفر یک Repo Node/Go/Python را در ۵ دقیقه از صفر (VPS خالی) به URL با TLS می‌رساند، لاگ می‌بیند، Shell می‌زند، Push بعدی Auto-deploy می‌شود، Rollback می‌کند، و بعد از Restart کردن Kuben هیچ‌چیز خراب نمی‌شود. RSS Idle < ۳۰MiB؛ Image < ۳۰MB. Soak Test ۱ ساعته لاگ بدون رشد حافظه.

### ۵.۲ اولویت‌بندی D1 اصلاح‌شده

**D1-Structural (فاز ۰، غیرقابل مذاکره):** مرز داده (۲.۲)، Cookie Session + Token Opaque، Projection، LogHub Bounded، Terminal Backpressure، SSA + Idempotent Reconcile، BuildRun/Release به‌عنوان CRD، OpenAPI Pipeline، Bootstrap Order، CRD Self-apply، Soft-delete Environment، Structured Logging + Request ID، Health `/livez`/`/readyz`، Graceful Shutdown، Budgetها در CI.

**D1-Stub (Trait در فاز ۰، پیاده‌سازی بعداً):** `IdentityProvider` (Local اکنون، OIDC بعد)، `LeaderElector` (Noop اکنون)، `PolicyEngine` (Static اکنون، Cedar بعد)، `MetricsSource` (metrics-server اکنون، kubelet بعد)، `NotificationSink` (Log اکنون)، `BlobStore` (فایل اکنون، S3 بعد)، `ClusterRegistry` (یک Cluster اکنون).

**D2:** بقیه‌ی بخش ۸ سند v1.0.

### ۵.۳ Roadmap بازنگری‌شده

| فاز | مدت (۳ نفر) | خروجی | Exit |
|---|---|---|---|
| **۰ — Skeleton** | ۶ هفته | ۶ Crate، Auth Local، Store دو-DB، CRD + Self-apply، OpenAPI → TS، UI Shell، CI با Budget، `install.sh` اولیه | `kuben serve` روی kind + Login + لیست Apps (خالی) + Budgetها سبز |
| **۱ — MVP** | ۶ هفته (جمعاً ۱۲) | جدول ۵.۱ | Exit MVP بالا؛ **۵ کاربر خارجی Alpha** |
| **۲ — Kubero Parity** | ۱۶ تا ۲۰ هفته | Git Providerها، Data Services (CNPG، Valkey، MariaDB)، Review Apps، Promotion، Notifications، Cron، Trivy، Custom Domains، Template Catalog + Importer Kubero، CLI، Helm، Zot اختیاری، `db migrate`، Docs (Astro)، i18n | مهاجرت یک نصب واقعی Kubero؛ Public Beta |
| **۳ — Enterprise** | ۱۲ تا ۱۶ هفته | OIDC/Passkeys/Scoped RBAC، HA (Postgres + Leader + N API)، Multi-cluster، Quota/NetPol/PSA کامل، Air-gap، OTel، Session Recording، Merkle Audit Anchor، CNB Integration، Self-upgrade | Chaos Suite سبز؛ Restore آزمایش‌شده؛ ۱.۰ |
| **۴ — Differentiators** | ادامه‌دار | Compose Import، MCP، Terraform، GitHub Action، GitOps دوطرفه، Scale-to-zero، Cedar | — |

**جمع تا ۱.۰: حدود ۴۵ تا ۵۵ هفته.** این عدد را در README ننویسید؛ Milestoneها را بنویسید.

### ۵.۴ سه Spike قبل از فاز ۰ (هر کدام ۲ تا ۳ روز)

این‌ها فرضیات پرریسک‌اند؛ اگر یکی شکست بخورد، پلن عوض می‌شود:

1. **Spike-A (RSS):** یک Binary با kube + axum + sqlx + rustls که ۶ Kind را با Projection Watch کند روی kind با ۲۰۰ Pod → RSS Idle را با سه Allocator اندازه بگیرید. اگر > ۴۰MiB، بودجه بازنگری شود.
2. **Spike-B (BuildKit Rootless):** buildkitd Rootless روی k3s پیش‌فرض + یک Managed Cluster (مثلاً GKE Autopilot که سخت‌گیر است). اگر روی Managed کار نکرد، Remote Build Fallback به فاز ۱ می‌آید.
3. **Spike-C (Terminal Backpressure):** `yes` داخل Container با Browser Throttled → مصرف حافظه‌ی Server ثابت بماند و Ctrl-C در کمتر از ۵۰۰ms اثر کند.

---

## ۶. ADRهای اضافه‌شده

| ADR | عنوان | تصمیم |
|---|---|---|
| 013 | یک Runtime در فاز ۰؛ Bulkhead پشت Config | جایگزین ADR-003 |
| 014 | یک SSE برای هر تب با Scope در URL + WS فقط Terminal | Transport Rule |
| 015 | مالکیت داده: SQL ↔ CRD با `uid` و BindingGC | جایگزین ADR-001 |
| 016 | buildkitd پایدار + Job سبک `buildctl`؛ Registry خارجی در MVP | جایگزین ADR-009 |
| 017 | CRD Self-apply در Boot + Ratcheting + Storage Migration | Lifecycle CRD |
| 018 | Soft-delete Environment با Grace Period و `deletionPolicy` | ایمنی حذف |
| 019 | Bootstrap Order و Polling Fallback برای Webhook | Zero-Ops واقعی |
| 020 | Audit Append-only در هسته؛ Merkle Anchor در Enterprise | جایگزین Hash-chain |
| 021 | ۶ Crate در فاز ۰؛ معیار Split | ساختار |
| 022 | Threat Model و RBAC دو-Role در HA | امنیت |

---

## ۷. Definition of Done معماری قبل از اولین خط کد

- [ ] ADRهای 001 تا 022 نوشته و Review شده‌اند (هر کدام حداکثر یک صفحه: Context، Decision، Consequences).
- [ ] سه Spike بخش ۵.۴ اجرا و نتیجه‌شان در ADR مربوطه ثبت شده است.
- [ ] Threat Model (بخش ۳.۷) در `docs/security/threat-model.md` است و هر کنترل به یک Test E2E منفی نگاشت شده است.
- [ ] Bootstrap Order (بخش ۳.۱) به‌صورت یک Runbook نوشته و روی VPS خالی دستی اجرا شده است.
- [ ] Schema اولیه‌ی OpenAPI برای MVP (فقط Endpointها، بدون پیاده‌سازی) نوشته شده و Frontend می‌تواند با MSW Mock شروع کند.
- [ ] CRD `v1alpha1` با CEL و `kubectl explain` معنادار تولید شده و یک `kubectl apply` نمونه روی kind کار می‌کند.
- [ ] Budgetهای بخش ۱۳ سند v1.0 به‌عنوان GitHub Action Gate پیاده شده‌اند (حتی اگر Binary هنوز Hello World است).
- [ ] یک `CONTRIBUTING.md` با Invariantهای ۶گانه (بخش ۱.۴ سند v1.0) به‌عنوان Review Checklist.

---

## ۸. جمع‌بندی

v1.0 معماری درستی را توصیف می‌کرد اما «معماری یک تیم ۱۰ نفره با ۱۸ ماه بودجه» بود. v1.1 همان جهت را با این تغییرات قابل‌اجرا می‌کند:

1. **ساده‌تر:** یک Runtime، یک SSE برای هر تب، ۶ Crate، Audit بدون Chain.
2. **صادق‌تر:** Registry خارجی در MVP، Kuben = Cluster-admin-lite، Bootstrap با sslip.io، زمان‌بندی ۱۲ تا ۱۸ ماه تا ۱.۰.
3. **ایمن‌تر:** Soft-delete Environment، Secret Reference در Release، Session Cache ۵ ثانیه‌ای، Threat Model صریح.
4. **سریع‌تر برای کاربر:** buildkitd با Cache پایدار — تنها تغییری که کاربر مهاجرت‌کرده از Coolify فوراً حس می‌کند.
5. **قابل‌سنجش:** سه Spike قبل از تعهد، MVP ۱۲ هفته‌ای با Exit صریح، و DoD معماری.

اگر فقط یک توصیه از این سند اجرا شود: **MVP بخش ۵.۱ را با Scope بسته بسازید و ۵ کاربر واقعی بگیرید قبل از اینکه یک خط از فاز ۲ نوشته شود.** بهترین معماری دنیا هم اگر ۱۸ ماه بدون کاربر بماند، به سرنوشت Rewriteهای ناتمام دچار می‌شود.
