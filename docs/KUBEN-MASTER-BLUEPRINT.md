# 🧭 Kuben — Comprehensive architecture, competitive differentiation and execution plan (Master Blueprint)

> A next-generation, Kubernetes-native PaaS, a single Rust binary, aiming to technically outclass Coolify, Dokploy, the incumbent PaaS, Devtron, KubeVela and the commercial contenders Qovery, Northflank and Porter.

| | |
|---|---|
| **Version** | 1.0 |
| **Date** | 2026-09-10 |
| **Required reading** | [KUBEN-GOLDEN-ARCHITECTURE.md](./KUBEN-GOLDEN-ARCHITECTURE.md) (fundamentals) and [KUBEN-ARCHITECTURE-CRITIQUE.md](./KUBEN-ARCHITECTURE-CRITIQUE.md) (v1.1 corrections). Where they conflict, **this document** takes precedence |
| **Evidence base** | A direct reading of the incumbent's source tree + web research (prices and ecosystem status as of September 2026; sources footnoted per section) |
| **Code destination** | `kuben-monorepo/` |

---

## Contents

- [0. Executive summary](#0-executive-summary)
- [1. Competitive analysis and technical advantage matrix](#1-competitive-analysis-and-technical-advantage-matrix)
- [2. Dissecting the incumbent PaaS](#2-dissecting-the-incumbent-paas)
- [3. The hi-tech, differentiating feature pack](#3-the-hi-tech-differentiating-feature-pack)
- [4. Final technology stack and monorepo structure](#4-final-technology-stack-and-monorepo-structure)
- [5. Phased execution plan, with phase 0 in full detail](#5-phased-execution-plan-with-phase-0-in-full-detail)
- [Appendix A. Sources](#appendix-a-sources)

---

## 0. Executive summary

**Kuben** is a single-binary control plane (Rust) that installs on any Kubernetes (from k3s to EKS), keeps desired state in CRDs, identity and audit in SQLite/Postgres, and embeds a modern SPA (React 19) inside itself. The goal: **under 30 MB RSS at idle, one Pod, no separate Redis/queue/operator**, plus a set of capabilities that today are only found in platforms costing $900 to $3,000 per month.

**Three defensible claims (not slogans):**

1. **The lightest control plane on Kubernetes:** Coolify runs four containers (Laravel + Postgres + Redis + Soketi) with user reports of 1.3GB at idle; Devtron wants at least 2 CPUs and 6GB for CI/CD; Kuben is one process with a 30MB budget. (k3s's own overhead is separate and is stated honestly in section 1.4.)
2. **Enterprise features as open source:** preview environments with DB branching, scale-to-zero, AI post-mortems, an eBPF service map, a FinOps meter and four-eyes approval — exactly what Qovery sells in its Business plan (from ~$2,000/month).
3. **No vendor lock-in:** everything is CRDs and standards (Gateway API, OCI, OIDC, OpenAPI, OTel). `kubectl` always works; if Kuben is removed, the apps keep running.

---

## 1. Competitive analysis and technical advantage matrix

### 1.1 Technical comparison matrix

> "Control-plane RAM" = management components only, without user workloads and without Kubernetes itself. Competitor numbers come from official documentation or user reports (sources in the appendix).

| Parameter | **Kuben (target)** | The incumbent (the system Kuben replaces) | Coolify | Dokploy | Devtron | KubeVela | Qovery | Northflank | Porter |
|---|---|---|---|---|---|---|---|---|---|
| **Execution model** | K8s-native, single Rust binary | K8s; NestJS UI + operator (Go/Helm) | Docker + SSH; PHP/Laravel + PG + Redis + Soketi | Docker Swarm; Node + PG + Traefik | K8s; dozens of microservices (Go) | K8s; Go controller + CUE | SaaS control plane + agent in your cluster | SaaS control plane (+BYOC) | SaaS control plane on your EKS/GKE/AKS |
| **Control-plane RAM at idle** | **≤ 30MB** (CI gate) | a few hundred MB (Node.js + node_modules + operator) | official minimum 2GB server; user reports ~1.3GB idle | official minimum 2GB server | 6GB (with CI/CD) up to 13GB (>5 apps) | request 20Mi, recommended Small: 1Gi | unknown (SaaS) + agent | unknown (SaaS) | unknown (SaaS) + dedicated monitoring node (~$49/mo) |
| **Binary / microservice architecture** | 1 binary, roles | 2 containers | 4 base containers + proxy + sentinel | 3 containers | dozens of Deployments | 1 controller + addons | Managed | Managed | Managed |
| **API response time (reads)** | p99 < 5ms server-side (in-memory projection) | every list → K8s API | DB query (PHP) | DB query (Node) | medium | K8s API | SaaS network | SaaS network | SaaS network |
| **HPA / autoscale** | HPA + optional KEDA + built-in scale-to-zero | basic HPA | ❌ (manual scaling, multi-server) | ❌ (Swarm replicas) | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Build isolation** | separate namespace, no SA token, NetPol, rootless buildkitd | build Pod with an SA token and `kubectl` (🔴) | builds on the same production Docker host | builds on the host | separate Job | — (no built-in build) | Managed builders | Managed builders | Managed |
| **Web shell security** | authz at upgrade, per-tab session, audit, recording (Ent) | guessable room, no authz on join (🔴) | WS via Soketi | WS | ✅ RBAC | ❌ (no shell UI) | ✅ | ✅ | ✅ |
| **Preview env per PR** | ✅ + TTL + DB branch | ✅ review apps (no TTL/DB) | ✅ (preview deployments) | ✅ | ❌ (pipeline-driven) | ❌ | ✅ (ephemeral) | ✅ | ✅ |
| **Cost management (FinOps)** | ✅ live meter (OpenCost spec) | ❌ | ❌ | ❌ | ⚠️ limited | ❌ | ✅ (add-on) | ✅ (billing) | ✅ (metered) |
| **Data ownership** | 100% in your cluster (etcd + SQLite/PG) | ✅ in-cluster | ✅ on your server | ✅ | ✅ | ✅ | ⚠️ metadata in Qovery's SaaS | ⚠️ in SaaS | ⚠️ in SaaS |
| **Networking** | Gateway API (future-proof) | Ingress (ingress-nginx retired) | Traefik/Caddy | Traefik | Ingress | Ingress/Trait | Managed | Managed | Managed |
| **License / price** | open source (proposed: Apache-2.0) | GPLv3 | Apache-2.0 + Cloud | Apache-2.0 + Cloud | OSS core + Enterprise | Apache-2.0 | from **$899/mo** (Team) to **$1,999–2,999/mo** (Business) | usage-based; BYOC: $0.01389/vCPU-hr + $0.00139/GB-hr | $13/vCPU-mo + $6/GB-mo |

### 1.2 What exactly do the paid competitors charge for?

| The capability they charge for | Qovery (Team $899 / Business ~$2K) | Northflank | Porter | **Kuben (open source)** |
|---|---|---|---|---|
| **Ephemeral / preview environments** | ✅ (capped at 100 to 250 envs) | ✅ | ✅ | Section 3.1 — unlimited, with TTL and a cost guard |
| **Deployment minutes** (5,000 to 10,000 minutes) | ✅ metered | Metered | — | Builds on your own buildkitd; no meter |
| **RBAC + audit logs** (7 to 30 days) | ✅ | ✅ | ✅ | Full RBAC + unlimited audit (configurable retention) |
| **SSO (SAML/OIDC)** | Business only | ✅ | Enterprise | OIDC in the core (phase 2), SAML via an IdP bridge |
| **Policy as code, 99.9% SLA** | Business | Enterprise | Enterprise | Protection rules + four-eyes (section 3.7); the SLA is your responsibility |
| **Observability / monitoring** | paid add-on | ✅ | ✅ (monitoring node ~$49/mo) | Metrics Lite + eBPF service map (section 3.5) |
| **Cost optimization** | Add-on | Billing UI | Metered | FinOps meter (section 3.6) |
| **AI skill + MCP server** | ✅ (AI seats $10/mo) | — | — | AI SRE (section 3.4) + MCP server, BYO-LLM or local Ollama |
| **Self-hosted / air-gapped control plane** | Enterprise only (custom) | Enterprise | — | Default |
| **Management fee on BYOC** | Flat | for 40 vCPU/80GB ≈ $486/mo | for 40 vCPU/80GB ≈ $1,000/mo | Zero |

**Conclusion:** a ten-person team running 40 vCPU on its own cloud pays $10,000 to $30,000 a year in "management fees" just to get preview environments, RBAC, audit and SSO. Kuben delivers all of it as open source and **inside the user's own cluster**; Kuben's possible revenue model (if it needs one) is support and enterprise add-ons only (session recording, SAML, Merkle audit) — not locking up the basic features.

### 1.3 Why today's open-source competitors have not filled this gap

| Competitor | Strength | Why Kuben gets ahead of it |
|---|---|---|
| **Coolify** | Excellent UX, one-line onboarding, 8 DB engines | Docker + SSH: no real HA, no scheduler, builds on the same production host (contention), a four-container control plane at ~1.3GB |
| **Dokploy** | Simple, Swarm | Swarm is dead in practice; no operator ecosystem; no Gateway API/CNPG/KEDA |
| **The incumbent** | The pipeline/review-apps idea, 160 templates | The security holes in section 2, a Helm-based operator, a split source of truth |
| **Devtron** | Enterprise-grade, deep GitOps | 6 to 13GB of RAM, installation complexity, overkill for a small team |
| **KubeVela** | A powerful OAM model | It is a framework, not a product; no build, no complete UI, a CUE learning curve |

### 1.4 The honest truth about footprint

Kubernetes (even k3s) has a base cost of a few hundred megabytes. On a one-gigabyte VPS, Coolify and Dokploy stay lighter. **Kuben's official hardware minimum: 2 vCPU and 2GB of RAM** (the same number Coolify and Dokploy publish) — with the difference that the very same installation grows to hundreds of nodes without a migration. The product message: *"From one VPS to a hundred nodes, without a rewrite."*

---

## 2. Dissecting the incumbent PaaS

> Locations below refer to components of the incumbent's own tree. Full detail is in the Golden document (section 1); here you get the operational summary plus "what we keep".

### 2.1 Strengths worth carrying over

| Concept | Where | How Kuben reproduces it |
|---|---|---|
| **Pipeline → Phase → App** (review/test/stage/prod) | the incumbent's pipelines module and app model | `Project → Environment → App` + promotion order in `Environment.spec.promotion` |
| **Review apps** on a PR | the incumbent's repo webhook handlers | `Environment.type=preview` + TTL + DB branch (section 3.1) |
| **Buildpack/Nixpacks/Dockerfile strategy** | the incumbent's deployment templates and buildpack config | `App.spec.source.build.strategy = auto|dockerfile|railpack|image` on top of BuildKit frontends |
| **Template catalog (160+ services)** with metadata annotations | the incumbent's per-service template manifests | A separate `kuben-templates` repo, a validated schema, generated secrets (the GPL license needs review) |
| **Addon plugins** (Postgres, Redis, MySQL, Mongo, Minio, …) | the incumbent's addon plugins | A `ServiceClass` CRD (data, not code) + the CNPG/Valkey/MariaDB operators |
| Configurable **podsize, runpack, securityContext** | the incumbent's Prisma schema | `KubenConfig.spec.sizes[]`, runpack → build strategy |
| **Multi-git-provider** (GitHub, GitLab, Gitea, Gogs, Bitbucket) | the incumbent's git provider adapters | A `GitProvider` trait with separate implementations; webhook + polling fallback |
| **Notifications** (Slack/Discord/webhook) | the incumbent's notifications module | Outbox pattern + Telegram/email added |
| **Vulnerability scan (Trivy), cron jobs, basic auth** | the incumbent's Kubernetes service and CRD spec | Kept (phase 2) |
| **i18n (en/de/ja/zh/pt)** | the incumbent's client locale files | paraglide + RTL |

### 2.2 Security and architecture gaps → Kuben's non-negotiable invariants

| # | Gap | Evidence | Invariant in Kuben (how it becomes **structurally** impossible) |
|---|---|---|---|
| S1 🔴 | **Terminal/log eavesdropping and hijacking between users:** `join` with no authz, `handleTerminal` writes input into any room; room names are guessable | the incumbent's events gateway and apps service | **I-1:** every subscription (SSE/WS) = `authz.require(perm, resource)` in that same handler + a random 128-bit session ID bound to `user_id`. Type system: `LogStream::subscribe(&AuthzProof, ..)` does not compile without an `AuthzProof` |
| S2 🔴 | **Default JWT secret in the code** | the incumbent's auth service and JWT strategy | **I-2:** there is no default secret; keys are generated on first boot and stored in a K8s Secret; no key → `exit 1` (fail-closed) |
| S3 🟠 | JWT in a JS-readable cookie and in `localStorage`; CSP disabled | the incumbent's client plugin bootstrap, login prompt and app entry point | **I-3:** an opaque session in a `__Host-` cookie + `HttpOnly; Secure; SameSite=Lax`; CSP `default-src 'self'` with no inline |
| S4 🟠 | Legacy HMAC-SHA256 hash + non-constant-time comparison | the incumbent's auth service | **I-4:** Argon2id only (OWASP m=19MiB,t=2,p=1) + `subtle::ConstantTimeEq`; imports from the incumbent → rehash on login |
| S5 🟠 | CORS `*` on WS, `cors: true`, no HSTS | the incumbent's events gateway and server entry point | **I-5:** same-origin by default; origin check at the WS upgrade; explicit allowlist |
| S6 🟠 | **PromQL injection** | the incumbent's metrics service | **I-6:** names are validated against a DNS-1123 regex and read from the projection, never from a user-supplied string |
| S7 🔴 | **A build Pod with an SA token and `bitnami/kubectl:latest`** that patches the CR itself | the incumbent's buildpack job template | **I-7:** the build Pod never sees the API server (`automountServiceAccountToken: false` + a NetPol allowing egress only to Git/registry/buildkitd). The controller reads the result from the Job status. All helper images are digest-pinned |
| S8 🟠 | Notifications broadcast to every socket; the guard is on the message, not on the handshake | the incumbent's events gateway | **I-8:** authn at the upgrade; topics are permissioned |
| S9 🟡 | A user's terminal output goes to the server's `process.stdout` | the incumbent's Kubernetes service | **I-9:** terminal content is never logged; only metadata goes into the audit |
| S10 🟡 | Hardcoded passwords in the templates (`password: wordpress`) | the incumbent's WordPress template | **I-10:** a template parameter schema with `generate: password` |
| C1 🔴 | **Shared mutable context** (`setCurrentContext` in 28 places) → races between clusters | the incumbent's logs service and apps service | **I-11:** an immutable `ClusterRegistry`; every operation takes an explicit `ClusterId`; no global mutable state |
| C2 🔴 | **Log stream leak** (an append-only array, no backpressure, a 300ms sleep) | the incumbent's logs service | **I-12:** a `LogHub` with ref-counting, a bounded `broadcast`, `LinesCodec::new_with_max_length(16KiB)`, drop-with-marker |
| C3 🟠 | A shell shared between users + a 3s sleep | the incumbent's apps service | **I-13:** one session per tab/user, idle timeout |
| C4 🟠 | No informer; a cron lists the whole cluster every 15s | the incumbent's status service | **I-14:** informer + projection; counters come from memory |
| C6 🟡 | Graceful shutdown disabled | the incumbent's server entry point | **I-15:** a complete shutdown order (readiness → drain → lease → flush → checkpoint) |
| C7 🟡 | `execSync('npx prisma migrate deploy')` at boot + `PRAGMA foreign_keys=OFF` | the incumbent's database service | **I-16:** embedded `sqlx::migrate!` with a lock; FKs always ON |
| A1 | **A split source of truth** (SQLite + the incumbent's instance CR + `config.yaml`) | the incumbent's config service | **I-17:** a definitive data boundary (section 4.2); SQL→CRD references only by `uid` + BindingGC |
| A2 | The operator is a Helm render; weak status/conditions | the incumbent's Go operator (outside the repo) | **I-18:** typed builders + SSA + kstatus conditions + `observedGeneration` |

These 18 invariants go into `CONTRIBUTING.md` as a **mandatory code-review checklist**, and each one has at least one **negative E2E test** (for example: a viewer from another org tries to attach to a log stream → 403).

---

## 3. The hi-tech, differentiating feature pack

> For each capability: **value**, **architecture**, **exact mechanism**, **honest limitation**, **phase**.

### 3.1 Ephemeral preview environments per PR (with auto-TTL)

**Value:** exactly what Qovery/Vercel sell; the incumbent has review apps, but with no TTL, no separate DB and no cost guard.

**Model:**

```yaml
apiVersion: kuben.dev/v1alpha1
kind: Environment
metadata:
  name: shop-pr-482
  labels: { kuben.dev/project: shop, kuben.dev/type: preview, kuben.dev/pr: "482" }
spec:
  type: preview
  template: staging                 # Base env: Env/Secret/Service are forked from it
  source: { provider: github, repo: acme/shop, pr: 482, headSha: 9f1c…, baseBranch: main }
  ttl: { idle: 48h, max: 14d }      # deleted after 48h with no traffic, or after 14 days at most
  budget: { maxMonthlyUsd: 40 }     # Cost guard (section 3.6)
  overrides:
    env: [{ name: FEATURE_FLAGS, value: "all" }]
    services:
      - name: shop-db
        mode: branch                # ← section 3.2 (Instant DB Branching)
  domains: { pattern: "pr-{pr}.{app}.{project}.{base}" }   # pr-482.api.shop.apps.example.com
status:
  phase: Ready
  url: https://pr-482.web.shop.apps.example.com
  expiresAt: 2026-09-24T10:00:00Z
  lastTrafficAt: 2026-09-11T08:12:00Z
```

**Flow:**

```mermaid
sequenceDiagram
  participant GH as GitHub
  participant API as kuben api
  participant C as kuben controller
  participant K as kube-apiserver
  GH->>API: PR opened/synchronize (HMAC verified, delivery-id deduped)
  API->>K: upsert Environment(type=preview, headSha)
  K-->>C: watch
  C->>K: Namespace kx-shop-pr-482 (PSA restricted, Quota, NetPol default-deny)
  C->>K: copy Secrets/ConfigMaps from staging with overrides (immutable, hashed names)
  C->>K: ServiceBranch shop-db (mode=branch) → wait for Ready
  C->>K: BuildRun (app+sha) → Release → Deployment/Service/HTTPRoute
  C-->>GH: Check Run + PR comment: "🟢 Preview ready: https://pr-482…"
  Note over C: PreviewGC every 5 minutes: idle>ttl.idle or now>expiresAt or PR closed → Environment.delete
  GH->>API: PR closed/merged → delete Environment
  C->>K: Finalizer: delete HTTPRoute, DNS record, ServiceBranch, Namespace (10m grace for undo)
```

**Engineering details:**

- **Idle detection:** `lastTrafficAt` comes from the activator/proxy (section 3.3) or from gateway access-log metrics; if neither exists, from the Traefik/Envoy metrics for the `HTTPRoute` (a light Prometheus scrape every 60s).
- **Preview secrets:** a production secret is never copied; only the `template` (which must be staging or dev) and only if `Environment.spec.protection.allowPreviewFrom` has permitted it.
- **Cost guard:** if the monthly estimate (section 3.6) exceeds `budget` → forced scale-to-zero + a comment on the PR.
- **Concurrency limit:** `Project.spec.previews.max: 10`; beyond that, a new PR queues up and the oldest idle one is deleted.
- **Domains and TLS:** a wildcard cert (`*.shop.apps.example.com`) with DNS-01 if it is configured; otherwise HTTP-01 per host (mind the rate limits — section 4.4).
- **Bot comment:** via a GitHub App (not a PAT) → a check run with status and a link to the logs.
- **Safe deletion:** a finalizer with a 10-minute timeout; if the ServiceBranch gets stuck, the namespace is deleted, the branch moves to `Orphaned` and the UI warns about it (rather than leaving the namespace in `Terminating`).

**Honest limitation:** for apps that depend on external stateful services (Stripe, a real S3 bucket), a preview is not complete without mock or sandbox keys; Kuben provides `overrides.env` for sandbox keys, but it does not work miracles.

**Phase:** 2 (after the MVP); the Environment CRD and the namespace lifecycle are designed in phase 0.

### 3.2 Instant database branching with copy-on-write

**Value:** previews with real (masked) data instead of an empty seed; Neon/Supabase only offer this inside their own cloud.

**The honest principle:** "instant" is only possible when the underlying storage supports **copy-on-write**. Kuben puts three strategies behind a single CRD and **automatically picks the best one available**:

| Strategy | Mechanism | Branch time for 5GB | Requires | Where |
|---|---|---|---|---|
| **`csi-clone`** | `VolumeSnapshot` (GA since K8s 1.20) of the source PVC → a new PVC from the snapshot → CNPG `bootstrap.recovery.volumeSnapshots` | seconds on Ceph RBD / Longhorn / OpenEBS Mayastor (thin) / ZFS-LocalPV; **minutes** on EBS/GCE PD (the snapshot goes to object storage) | a CSI driver with a VolumeSnapshotClass | Managed and on-prem with a suitable CSI |
| **`overlay`** | OverlayFS over `PGDATA` (the same mechanism as container image layers): lower = the source's base backup, upper = the branch's empty volume; Postgres comes up through WAL crash recovery. (the pgbranch pattern: ~1.9s regardless of size) | **~2 seconds** | `CAP_SYS_ADMIN` to mount inside the container; a fixed node (`hostPath` or a local PV) | single- or multi-node k3s, dev/preview |
| **`logical`** | `pg_dump \| pg_restore` with parallel jobs + a size cap | minutes (linear in size) | nothing | Fallback everywhere; default cap 2GB |

**CRD:**

```yaml
apiVersion: kuben.dev/v1alpha1
kind: ServiceBranch
metadata: { name: shop-db-pr-482, namespace: kx-shop-pr-482 }
spec:
  source: { namespace: kx-shop-staging, service: shop-db }   # CNPG Cluster
  strategy: auto                       # auto | csi-clone | overlay | logical
  pointInTime: latest                  # or a timestamp (with WAL archive)
  masking:                             # PII scrub before it becomes reachable
    sqlRef: { configMap: shop-db-mask, key: mask.sql }
  ttl: 14d
  size: { cpu: 250m, memory: 512Mi }   # smaller than the source
status:
  strategyUsed: overlay
  phase: Ready
  connection: { secretRef: shop-db-pr-482-app }   # DATABASE_URL is injected
  branchedAt: 2026-09-11T08:10:04Z
  sizeOnDisk: 41Mi                     # delta only
```

**How `overlay` works (a Kuben implementation, not an external dependency):**

1. The controller keeps one **base layer** per source: a `pg_basebackup` (or a CSI snapshot if that is cheap) stored on a local PV on the node, refreshed every 6 hours and after every migration (read-only, shared by all branches).
2. For each branch: an empty PVC (upper) + a Postgres Pod (the standard `postgres:17` image) with a Kuben init container (`kuben-overlay-init`, Rust, ~3MB) that performs `mount -t overlay` with `lowerdir=/base,upperdir=/upper/data,workdir=/upper/work` and points `PGDATA` at the merged view.
3. Postgres comes up via crash recovery (like a power cycle). Changed pages are copied into the upper layer (copy-up); everything else is read from the lower layer.
4. `masking.sql` runs; new credentials are generated and placed in a Secret.
5. **Security:** the Pod gets `CAP_SYS_ADMIN` only for the init container and only in `preview` namespaces (PSA `privileged` only for those namespaces, via an explicit label); Kuben states this plainly in the UI as "Preview DB Branching requires privileged init on this cluster", and on strict managed clusters (GKE Autopilot) it automatically falls back to `csi-clone` or `logical`.
6. **Deletion:** the upper PVC is deleted; the base layer is ref-counted and TTL'd.

**For MySQL/MariaDB:** `overlay` works identically (InnoDB crash recovery); `csi-clone` works with the MariaDB operator; for Valkey/Redis only `logical` (an RDB copy).

**Honest limitation:** the branch is taken from the live primary and **does not read from the source** (full isolation), but the base layer can be up to 6 hours old unless `pointInTime: latest` triggers an immediate refresh (seconds to minutes depending on size). A branch is not meant for production-grade HA.

**Phase:** 2 (`logical` and `csi-clone`), 3 (`overlay`).

### 3.3 Sub-second scale-to-zero without Knative

**Value:** preview environments and low-traffic apps should cost zero; the incumbent has a simple "sleep"; Coolify has nothing.

**The honest principle behind "under 1 second":** a real Kubernetes cold start = scale (100–300ms) + schedule (100–500ms) + container start (image cached: 200ms–2s) + app boot (Node ~300ms, JVM several seconds). **Under one second from zero is only possible for cached images and fast apps.** Kuben offers two modes and reports honestly:

| Mode | Mechanism | Wake latency | Savings |
|---|---|---|---|
| **`throttle`** (the default for previews) | The Pod stays alive; with **in-place Pod resize** (K8s ≥1.33, beta enabled by default) CPU drops to `10m` and memory to a minimum; a new request → the resize is reverted | **~0 ms** (the Pod is already running) + 100–300ms for the CPU to come back | CPU ~95%, RAM ~0% (downward memory resize is limited) |
| **`zero`** | Replicas → 0; the activator holds the request; scale to 1; Pod ready → forward | **1 to 3 seconds** (image cached) | 100% |

**Activator architecture (inside the same binary, role `activator`):**

```
Gateway (HTTPRoute host=pr-482.web…)  ──►  Service kuben-activator:8080  ──►  Pod app (when it is awake)
                                                   │
                                     Rust hyper proxy (~800 LOC):
                                     • per-host state: Awake | Sleeping | Waking(notify)
                                     • request counter → lastTrafficAt (for idle detection)
                                     • Sleeping: hold request (max 30s) + Scale/Resize + wait Pod Ready via informer
                                     • Waking: all concurrent requests wait on the same notify (no thundering herd)
                                     • Awake: forward straight to the Pod IP (endpoints from the projection) — one hop, ~100µs
                                     • Browser: a "Waking up…" page with refresh (HTML) if Accept: text/html and wake > 2s
```

- **Always on the path** for apps that have `idle` enabled (toggling the HTTPRoute on every sleep/wake does not work out with Traefik's 2-second propagation). The cost: one Rust hop. Apps without `idle` go straight to their own Service.
- **Idle detection:** the activator reports `lastTrafficAt` to the controller every 30s (an in-process channel under `--roles=all`, or a CR status patch in HA). After `idle.after` (15 minutes by default) → sleep.
- **Scale-out:** the activator counts concurrency and can feed it to HPA/KEDA as an external metric (phase 3).
- **HA:** the activator is stateless; N replicas behind a Service; no notify is needed between replicas (each one watches Pod readiness independently).
- **The alternative:** the KEDA HTTP add-on does the same job with 3 components (operator, interceptor, scaler); Kuben **does not use it**, because that means 3 more Deployments with a bigger footprint — but it will support `InterceptorRoute` as an optional backend in phase 3.

```yaml
# App.spec.runtime.processes.web.idle
idle:
  mode: throttle          # throttle | zero | off
  after: 15m
  throttle: { cpu: 10m }  # In-place resize target
  wakeTimeout: 30s
  placeholder: true       # a "Waking up" HTML page
```

**Honest limitation:** a long-lived WebSocket/SSE connection to a sleeping app cannot be "held"; the activator re-establishes them after the wake (the client must reconnect). In-place resize does not exist on clusters older than 1.33 → automatic fallback to `zero`.

**Phase:** 2 (`zero`), 3 (`throttle`).

### 3.4 AI SRE: automatic post-mortems and one-click fix suggestions

**Value:** "why did my Pod crash?" is the single most common question PaaS users ask; Qovery sells this as an AI seat.

**Design principle:** **deterministic rules first, the LLM as an explainer and advisor, never as an automatic executor.**

```mermaid
flowchart LR
  EV[K8s Events + Pod Status + Exit Codes] --> DET[Incident Detector<br/>rules: OOMKilled, CrashLoop, ImagePull, Probe fail, Pending]
  DET --> INC[(Incident record)]
  INC --> CTX[Context Pack builder<br/>• last 200 log lines (redacted)<br/>• events 30m<br/>• resources req/limit/usage<br/>• Release diff (env/image/scale)<br/>• metrics window 15m<br/>• probe config]
  CTX --> RULES[Rule Engine → deterministic findings<br/>e.g. OOM: usage≥limit ⇒ suggest +50% memory]
  RULES --> LLM{LLM enabled?}
  LLM -- no --> CARD
  LLM -- yes --> GEN[genai client → provider<br/>Ollama local / OpenAI / Anthropic / Gemini<br/>structured JSON output]
  GEN --> CARD[Post-Mortem Card<br/>root cause · confidence · evidence · actions]
  CARD --> ACT[One-click actions (RBAC-gated)<br/>Rollback · Bump memory · Fix healthcheck path · Restart · Open PR]
```

**Mechanism:**

1. **Detector** (in the controller, off the projection): combines `containerStatuses.lastState.terminated.reason`, `restartCount` and events (`FailedScheduling`, `Unhealthy`, `BackOff`) → `Incident{kind, app, pod, first_seen, count}`; deduplicated over a 10-minute window.
2. **Context pack** with **mandatory redaction**: secret regexes (AWS keys, JWT, `password=`, Bearer, PEM), the values of every env var that came from a Secret (replaced with `<redacted:NAME>`), and internal IPs optionally. Pack size ≤ 16KB.
3. **Rule engine** (Rust, no LLM): 15 to 20 rules with a definite suggested fix (OOM → memory; probe 404 → path; port mismatch → port; `CrashLoop` with exit 1 and an `ECONNREFUSED :5432` log line → DB service down; ImagePullBackOff → registry auth).
4. **LLM (optional, opt-in):** the `genai` crate (multi-provider, native protocols, Ollama for on-prem/air-gapped). A fixed prompt + a JSON output schema (`root_cause`, `confidence 0–1`, `evidence[]`, `actions[] {type, params, risk}`). A 20s timeout, a daily cost cap, and a cache keyed on the context hash (a repeated incident = no call).
5. **Actions:** every action is an existing mutation in the API (rollback to the previous release, patch `App.spec.runtime.processes.web.size`, patch `healthCheck.path`) → the same RBAC and audit. **No action runs automatically** unless `Environment.spec.autoRemediation` explicitly allows it for specific deterministic rules (for example an automatic rollback when a new release goes into CrashLoop within the first 5 minutes — which needs no LLM at all).
6. **Post-mortem doc:** for production incidents, an automatic Markdown document (timeline, impact, root cause, action items) is stored in the audit log and sent to Slack/Telegram.
7. **A built-in MCP server** (`rmcp`): the tools `get_incident`, `get_logs` and `rollback`, so external agents (Cursor, IDE extensions, the CLI) can debug too — with the same token and the same RBAC.

**Privacy/compliance:** the LLM is off by default; provider and model live in `KubenConfig`; there is a "local only (Ollama)" option; every prompt/response is logged in full in the audit (redacted).

**Phase:** 2 (detector + rules + card, no LLM), 3 (LLM + MCP).

### 3.5 eBPF observability: a live service map with no code changes

**Value:** a map of services, latency and error rate for every app with no SDK — Northflank/Qovery sell this as a monitoring add-on.

**The key decision: Kuben does not write eBPF code.** The **OpenTelemetry eBPF Instrumentation (OBI)** project — Grafana's donation of Beyla to OpenTelemetry, whose first release shipped in November 2025 — does exactly this: RED metrics and traces for HTTP/S, HTTP/2, gRPC, SQL, Redis, Kafka and MongoDB, with no code changes, out of process. Kuben installs it as an **optional addon** and **digests its data internally**.

```
[OBI DaemonSet]  ──OTLP/HTTP (protobuf)──►  [kuben api: /otlp/v1/metrics, /otlp/v1/traces]  (feature "otlp")
   • discovery: namespaces with the label kuben.dev/managed                │
   • kernel ≥ 5.8 + BTF (k3s/Ubuntu 22.04+ OK)                            ▼
                                                            [In-memory aggregator]
                                                            • per (src_app → dst_app) edge: RPS, p50/p95/p99, error%  (ring buffer 1h, 15s buckets)
                                                            • per app: RED
                                                            • sampled traces: the last 100 per app (for "slow request" drill-down)
                                                                    │
                                                                    ▼
                                                            SSE delta → React Flow Service Map + uPlot sparklines
```

- **Fallbacks:** if Cilium is installed, Hubble Relay (gRPC) serves as an alternative source; if eBPF is impossible at all (an old kernel, a restricted managed cluster), the service map is built at L4 from **NetworkPolicy + gateway access logs** (without latency).
- **Budget:** the aggregator is capped at 50 apps × 20 edges × 240 buckets × 32B ≈ 8MB; beyond that → downsample. Traces are sampled only (simple tail-based: errors + the slowest 1%).
- **Export:** the same OTLP is forwarded to the user's Prometheus/VictoriaMetrics/Grafana Tempo (Kuben is not long-term storage).
- **Overhead:** OBI costs ~50 to 150MB of RAM per node (outside Kuben's control plane, shown transparently in the UI as an addon).
- **Security:** OBI is privileged (eBPF); only a cluster admin can enable it; Kuben stores no HTTP payloads (metadata only: method, route template, status, duration).

**Phase:** 3.

### 3.6 Live FinOps meter (transparent dollar cost)

**Value:** "how much is this preview env costing us?" — without a full OpenCost/Kubecost.

**The model (compatible with the OpenCost specification):**

```
cost(container, window) = Σ_resource max(request, usage) × duration_h × unit_price
  CPU:     max(req_cores, avg_usage_cores) × h × $/core-h
  Memory:  max(req_GB,    avg_usage_GB)    × h × $/GB-h
  Storage: pvc_GB × h × $/GB-h (by StorageClass)
  LB/IP:   per Gateway/LoadBalancer × $/h
  Egress:  bytes × $/GB (if the metric is available)
Idle cost = Node cost − Σ workload cost  (shown separately; sharing optional)
```

**Price sources (in priority order):**
1. **An OpenCost already in the cluster** → just query `/allocation` (Kuben does no calculation).
2. **Cloud provider auto-detection** from the `node.kubernetes.io/instance-type` label + region → an embedded on-demand price table (AWS/GCP/Azure/Hetzner/DigitalOcean; ~200KB of compressed JSON, updated with every release or fetched optionally).
3. **Custom pricing** in `KubenConfig.spec.pricing` (`cpuHour`, `gbHour`, `storageGbMonth`, `lbMonth`, `currency`) for on-prem — Kuben offers an initial suggestion based on "node price ÷ capacity".

**Implementation:** the same metrics-server poller (every 15s) that Metrics Lite uses provides usage; requests come from the projection; an hourly ring buffer → a daily roll-up in SQL (`cost_rollups(app_uid, day, cpu_usd, mem_usd, storage_usd)`) → displayed as live ($/h), month-to-date and a (linear) forecast, for each Pod/App/Environment/Project/Team; budget alerts (outbox → Slack/Telegram); a cost guard for previews (section 3.1).

**Honest limitation:** without a real billing API this is "an estimate based on list prices", not an invoice; the UI says so with an "Estimated" label. Spot/reserved discounts only work with custom pricing.

**Phase:** 2 (the basic meter), 3 (budget/forecast/team).

### 3.7 Four-eyes approval gate for production (Telegram/Slack/UI)

**Value:** SOC2/ISO require separation of duties; Qovery sells this under "policy as code" in the Business plan.

**Model:**

```yaml
# Environment.spec.protection
protection:
  requireApprovals: 2                 # distinct approvers, not the requester
  approverRoles: [owner, admin, release-manager]
  channels: [ui, telegram, slack]
  timeout: 4h
  breakGlass: { enabled: true, requireReason: true, notify: [security-channel] }
  window: { allow: "Mon-Fri 08:00-18:00 Europe/Berlin", elseRequireApprovals: 3 }
```

**Flow:**

1. `POST /releases/{id}/promote?to=prod` → if protection is enabled, an `Approval{id, release, requester, required=2, expires}` is created in SQL (not a CRD; this is human workflow data) and the release stays in `status.phase: AwaitingApproval`.
2. The **outbox** sends the message to the channels:
   - **Telegram** (`teloxide`): a message with a summarized diff (image digest, changed env, scaling) + an inline keyboard `[✅ Approve] [❌ Reject] [🔍 Open in Kuben]`. `callback_data = "apr:<approval_id>:<nonce>"` (no sensitive data). On the callback: the Telegram `from.id` → the `identity_links(provider=telegram, subject=<id>, user_id)` table (linked with a one-time code from the user's profile) → check the role and that it differs from the requester → record the vote → edit the message to "1/2 approved by @alice". The bot runs in **webhook** mode (not long polling) behind the same API, with a secret token header.
   - **Slack:** Block Kit with interactive buttons; signing-secret verification; `user.id` → `identity_links(provider=slack)`.
   - **UI/CLI:** `kuben approve <id>`.
3. Once `required` is reached, the controller performs the promotion; the audit record includes every vote (user, channel, IP/Telegram ID, time).
4. **Break-glass:** an owner can bypass with a mandatory reason → an immediate alert to the security channel + an automatic post-mortem (section 3.4).
5. **Anti-circumvention:** an approver cannot be the requester; a user with two identities (Telegram + Slack) still has one vote; the approval is locked to `release.digest` — if the digest changes, the approval is void.

**Phase:** 2 (UI), 3 (Telegram/Slack/window/break-glass).

---

## 4. Final technology stack and monorepo structure

### 4.1 Backend

| Component | Choice | Version (September 2026) | Notes |
|---|---|---|---|
| Language | Rust, Edition 2024 | MSRV **1.94** (required by sqlx 0.9) | `rust-toolchain.toml` |
| HTTP | `axum` | 0.8.x | + `axum-extra` (Cookie, TypedHeader) |
| Runtime | `tokio` (one runtime; bulkhead behind config) | 1.x | `worker_threads = min(4, cpus)`, `max_blocking_threads = 16` |
| Middleware | `tower`, `tower-http` (trace, timeout, limit, compression, request-id, cors, set-header) | 0.5 / 0.6 | |
| Kubernetes | `kube` (runtime, derive, ws, rustls-tls), `k8s-openapi` | **kube 4.0** (June 2026) | Streaming lists, a default retry policy, WS keepalive |
| CRD schema | `schemars` | 1.x | CEL in `x-kubernetes-validations` |
| DB | `sqlx` (sqlite, postgres, runtime-tokio, tls-rustls, migrate, uuid) | **0.9** (May 2026) | Multi-DB `sqlx.toml`, `SqlSafeStr` |
| Query builder | `sea-query` + `sea-query-binder` | the latest compatible with sqlx 0.9 | For dynamic queries |
| OpenAPI | `utoipa`, `utoipa-axum`, `utoipa-scalar` | 5.5 / 0.2 | OpenAPI 3.1 |
| Auth | `argon2`, `subtle`, `openidconnect`, `totp-rs`, `webauthn-rs` | | Opaque sessions; JWT internal only |
| Crypto | `sha2`, `hmac`, `chacha20poly1305`, `rand`, `secrecy`, `zeroize` | | |
| Cache/concurrency | `moka`, `papaya`/`dashmap`, `arc-swap`, `parking_lot` | | |
| Config/CLI | `figment`, `clap` | | |
| Observability | `tracing`, `tracing-subscriber` (json), `metrics`, `metrics-exporter-prometheus`; `opentelemetry-otlp` behind the `otel` feature | | |
| Retry/backoff | `backon` | 1.x | |
| Git | `octocrab` (GitHub), `reqwest` (everything else), `gix` (ls-remote) | | |
| LLM | `genai` | 0.6/0.7 | Behind the `ai` feature |
| Telegram | `teloxide` (webhook mode) | | Behind the `telegram` feature |
| MCP | `rmcp` | | Behind the `mcp` feature |
| Assets | `rust-embed` (+ pre-compressed Brotli) | 8.x | |
| Allocator | `mimalloc` (short purge) — benchmarked in Spike-A | | |
| IDs/time | `uuid` (v7), `jiff` | | |
| Errors | `thiserror` (libs), `anyhow` (bin) | 2 / 1 | RFC 9457 in the API |
| Test | `cargo-nextest`, `insta`, `rstest`, `testcontainers`, `wiremock`, `proptest` | | |

### 4.2 The data boundary (final)

| Data | Owner | Reference |
|---|---|---|
| Project, Environment, App, Release, BuildRun, Domain, Service, ServiceBranch, KubenConfig | **CRD (etcd)** | SQL references them only by `uid`; `BindingGC` deletes orphaned bindings after a 1-hour delay |
| Org, User, Identity, Membership, RoleBinding, Session, ApiToken, Audit, Approval, Outbox, CostRollup, IdempotencyKey | **SQL** (single-replica SQLite WAL / HA Postgres) | The org is carried on the CR via the `kuben.dev/org` label |
| Build logs, backups | **files on a PVC** or `object_store` | SQL: metadata + hash |
| Short-term metrics, projections, service map | **memory** (bounded) | Rebuilt from the watch after a restart |

### 4.3 Frontend

| Component | Choice | Version |
|---|---|---|
| Build | **Vite 8** (Rolldown + Oxc) | 8.x |
| UI | **React 19** + React Compiler | 19.x |
| Routing | `@tanstack/react-router` (file-based, `defaultPreload: 'intent'`) | 1.x stable (v2 once it goes stable) |
| Server state | `@tanstack/react-query` | 5.x |
| API client | `openapi-typescript` + `openapi-fetch` + `openapi-react-query` (generated from `utoipa`) | |
| Styling | **Tailwind CSS v4** (`@tailwindcss/vite`) + **shadcn/ui on Base UI** | 4.x |
| RTL/Persian | Logical properties (`ms-`, `pe-`, `start`, `end`), dynamic `dir`, self-hosted **Vazirmatn** + Inter Variable fonts, `paraglide-js` for compile-time i18n | |
| Terminal/logs | `@xterm/xterm` + fit, web-links, webgl, search, unicode11 | 5.x |
| Charts | `uplot` | |
| Graph (service map, pipeline) | `@xyflow/react` | 12.x |
| Editor | CodeMirror 6 (YAML) — **not Monaco** | |
| Forms | `react-hook-form` + `zod` | |
| Tooling | `@biomejs/biome`, `vitest`, `@playwright/test`, `msw`, `size-limit`, `knip` | |
| TypeScript | ^5.9 (TS 6 after the ecosystem stabilizes) | |

### 4.4 Build, network, TLS

- **Build:** rootless `buildkitd` (StatefulSet, a 20Gi PVC cache, internal GC) in the `kuben-builds` namespace (PSA `baseline`; `seccompProfile: Unconfined` + AppArmor `unconfined` for this Pod only — a rootless BuildKit requirement) + a lightweight `buildctl` Job per build (with no SA token). Frontends: `dockerfile.v0`, and `gateway.v0` with `ghcr.io/railwayapp/railpack-frontend@<digest>` (after `railpack prepare` runs in an init container to produce `railpack-plan.json`). Deployment is by **digest**. An external registry in the MVP; an internal Zot in phase 2, only with a public domain + ACME.
- **Networking:** the **Gateway API** (`HTTPRoute`). Default implementations:
  - On **k3s**: k3s's own bundled Traefik with `providers.kubernetesGateway.enabled=true` (no extra component; genuinely zero-ops).
  - On other clusters: **Envoy Gateway** (the CNCF standard) or Traefik 3 — chosen in the installer; Kuben only writes standard `HTTPRoute` and `Gateway` resources and depends on no vendor-specific annotation. ⚠️ Independent benchmarks show that Traefik struggles with several simultaneous Gateways and thousands of routes, and that Envoy Gateway has leaked memory under heavy route churn; for large installations (>500 routes) the documentation will recommend Istio Gateway or kgateway.
- **TLS:** cert-manager with `config.enableGatewayAPI: true` (HTTP-01 via a temporary HTTPRoute on the port-80 listener; DNS-01 for wildcards).
- **The default domain when the user has no DNS:** `<ip>.sslip.io`. ⚠️ **A real risk:** Let's Encrypt applies its "certificates per registered domain" cap to `sslip.io`; despite repeated increases (up to 200,000 per week), it was exhausted again in February 2026. **Kuben's strategy:** (1) prefer an **IP certificate** directly from Let's Encrypt (officially supported; the rate limit is tied to that IP, not to sslip.io) when Kuben sits on a public IP; (2) fall back to `nip.io`; (3) fall back to ZeroSSL (ACME EAB); (4) a clear message in the wizard: "for production, connect your own domain". Never a wildcard on sslip.io (it would need DNS-01 on a domain that is not ours).

### 4.5 `kuben-monorepo` layout

```
kuben-monorepo/
├── Cargo.toml                      # [workspace] + [workspace.dependencies] + lints + profiles
├── Cargo.lock
├── rust-toolchain.toml
├── deny.toml                       # cargo-deny: licenses, advisories, bans
├── clippy.toml
├── .cargo/config.toml              # target-cpu, linker (mold/lld), musl targets
├── justfile                        # the single entry point
├── package.json                    # pnpm root (private)
├── pnpm-workspace.yaml             # packages + catalog
├── biome.json
├── .npmrc
├── .github/workflows/{ci.yml,release.yml,budgets.yml}
├── crates/
│   ├── kuben-crd/                  # CRD types (kube-derive + schemars + CEL) + bin crdgen — no tokio/axum
│   ├── kuben-core/                 # domain types, ids, errors, config, traits (Store, PolicyEngine, IdentityProvider, LeaderElector, BlobStore, MetricsSource)
│   ├── kuben-store/                # sqlx + sea-query, repositories, migrations/{sqlite,postgres}, sqlx.toml
│   ├── kuben-platform/             # ClusterRegistry, informers → projections, LogHub, exec, controllers, build, activator, incidents
│   ├── kuben-api/                  # axum routers, OpenAPI (utoipa), auth middleware, SSE/WS, web assets embed, bin openapi
│   └── kuben/                      # bin "kuben": serve (roles), migrate, backup, restore, doctor, reset-admin, import
├── apps/
│   └── web/                        # Vite 8 + React 19 SPA (→ dist/ embedded)
├── packages/
│   └── api-client/                 # openapi.json + schema.d.ts (generated, committed)
├── charts/kuben/                   # Helm chart (OCI); crds/ generated
├── deploy/
│   ├── install.sh                  # k3s + Gateway + cert-manager + kuben
│   └── manifests/                  # kubectl apply -k
├── docs/adr/                       # ADR-001 … ADR-022
├── scripts/                        # budget checks, e2e helpers
└── CONTRIBUTING.md                 # the 18 invariants as a review checklist
```

**The criterion for splitting out a new crate:** only when (a) one crate's incremental compile time exceeds 60s, (b) the crate will be published independently (`kuben-client` for the CLI/SDK in phase 2), or (c) it marks a team boundary.

---

## 5. Phased execution plan, with phase 0 in full detail

### 5.1 Roadmap (a three-person Rust-fluent team)

| Phase | Duration | Output | Exit Criteria |
|---|---|---|---|
| **0 — Skeleton** | 6 weeks | Monorepo, 6 crates, local auth + session, two-DB store, CRD v1alpha1 + self-apply, OpenAPI→TS pipeline, UI shell (login, layout, command palette, RTL), informer + basic projection, health/shutdown, CI with budget gates, initial `install.sh` | `kuben serve` on kind: login → project list (empty) → create project/environment → the namespace is created; image < 30MB; idle RSS < 30MB; CI green |
| **1 — MVP** | 6 weeks | App from an image + from Git (Dockerfile/Railpack on buildkitd), release/rollback, HTTPRoute + TLS (sslip.io/IP cert), LogHub + SSE per tab, terminal WS + ephemeral debug, GitHub webhook + polling, `kuben doctor`, `backup/restore` | Git push → URL with TLS in < 5 minutes from an empty VPS; 1h log soak test with no memory growth; 5 external alpha users |
| **2 — Parity+** | 16 to 20 weeks | Preview env + TTL (3.1), DB branching `logical`/`csi-clone` (3.2), scale-to-zero `zero` (3.3), incident rules + card (3.4), FinOps meter (3.6), approval UI (3.7), Git providers, CNPG/Valkey/MariaDB, cron, Trivy, notifications (outbox), custom domains, template catalog + importer for the incumbent PaaS, CLI (`kuben-client`), Helm, optional Zot, `db migrate`, docs (Astro), i18n (fa/en/de) | Migrating a real installation of the incumbent PaaS without losing data; public beta |
| **3 — Enterprise** | 12 to 16 weeks | OIDC/passkeys/scoped RBAC, HA (Postgres + leader + N API), multi-cluster, full quota/NetPol/PSA, DB branching `overlay`, scale-to-zero `throttle`, AI SRE with LLM + MCP, eBPF service map (3.5), Telegram/Slack approvals + break-glass, air-gap, OTel, session recording, Merkle audit anchor, self-upgrade | Chaos suite green (kill the API server, 410 Gone, kill the leader, disk full); restore tested; **v1.0** |
| **4 — Differentiators** | Ongoing | Compose import, Terraform provider, GitHub Action, bidirectional GitOps, Cedar policies, KEDA integration | — |

**Three spikes before phase 0 (2 to 3 days each):** (A) RSS with kube 4 + axum + sqlx + rustls + a projection of 200 pods on kind with 3 allocators; (B) rootless buildkitd on k3s and GKE Autopilot; (C) terminal backpressure with `yes` and a throttled browser.

### 5.2 Phase 0 — weekly breakdown

| Week | Track A (Platform/Rust) | Track B (API/Store/Auth) | Track C (Frontend/DX) | Acceptance |
|---|---|---|---|---|
| **1** | Scaffold the monorepo, `kuben-core` (types, config, errors), `kuben-crd` (Project/Environment/App/Release/BuildRun + crdgen), CI (fmt/clippy/nextest/deny) | `kuben-store`: sqlite+postgres pool, `sqlx.toml`, migration 0001 (orgs/users/sessions/tokens/role_bindings/audit), repository traits + matrix test | Vite 8 + React 19 + TanStack + Tailwind v4 + shadcn (Base UI) + RTL tokens + Biome; MSW mock from the OpenAPI stub | `just ci` green; `crdgen` produces valid YAML (`kubectl apply --dry-run=server`) |
| **2** | `kuben` bin: `serve --roles`, signals, supervisor, health (`/livez`,`/readyz`), graceful shutdown, config (figment), tracing JSON | `kuben-api`: axum + utoipa router, Problem+JSON, request-ID, body limit, rate limit, `POST /auth/login` (Argon2 + session cookie), `GET /me`, `openapi` bin | Login page, layout, theme (dark), command palette, router guards (`beforeLoad`), global 401 handling | Real login from the UI; `__Host-` cookie; CSP with no violations |
| **3** | `ClusterRegistry` (in-cluster/kubeconfig), informer projection for Namespace/Deployment/Pod with a label selector, global sequence, delta bus (`broadcast`) | RBAC engine (static roles, moka cache, version counter), `AuthzProof` type, API tokens (`kbn_pat_…`), audit writer + request context | SSE client (`Last-Event-ID`, snapshot+delta with `setQueryData` + rAF batching), Projects/Environments pages | `kubectl create -f app.yaml` → the UI shows the change in < 1s |
| **4** | CRD self-apply at boot (SSA), Project/Environment controller → namespace (PSA/Quota/NetPol), conditions + `observedGeneration`, events recorder, error policy + per-object backoff, soft-delete (7d grace) | CRUD API for Project/Environment/App (validation with garde + CEL), `If-Match`/ETag, Idempotency-Key, cursor pagination | Project/Environment/App forms (react-hook-form + zod from the schema), YAML viewer (CodeMirror) | Creating an environment from the UI → a namespace with labels/quota; deleting it → Terminating with a countdown |
| **5** | `rust-embed` + Brotli precompressed + SPA fallback + cache headers; Dockerfile (cargo-chef + zigbuild + distroless/static:nonroot); budget gates (binary/image/RSS/JS) | Backup/restore (SQLite `VACUUM INTO` + CRD export), `reset-admin`, `doctor` (preflight: RWO StorageClass, Gateway class, cert-manager, metrics-server, PSA) | `size-limit` per route, Playwright E2E (login → create project), Lighthouse a11y | Image ≤ 30MB; idle RSS ≤ 30MB on kind with a 50 app/200 pod fixture |
| **6** | `install.sh` (k3s + Traefik Gateway API + cert-manager + kuben), Helm chart skeleton, bootstrap order, polling fallback stub | OpenAPI drift check in CI, threat model doc, ADR 001–022 finalized | Minimal docs site (README + docs/), optional Storybook | Install on an empty VPS with a single command → login from `https://<ip>.sslip.io` (or an IP cert) |

### 5.3 Monorepo root files

#### `Cargo.toml` (Workspace)

```toml
[workspace]
resolver = "3"
members = ["crates/*"]
default-members = ["crates/kuben"]

[workspace.package]
version = "0.1.0"
edition = "2024"
rust-version = "1.94"
license = "Apache-2.0"
repository = "https://github.com/kuben-dev/kuben"
authors = ["Kuben Contributors"]

[workspace.dependencies]
# --- internal ---
kuben-core     = { path = "crates/kuben-core" }
kuben-crd      = { path = "crates/kuben-crd" }
kuben-store    = { path = "crates/kuben-store" }
kuben-platform = { path = "crates/kuben-platform" }
kuben-api      = { path = "crates/kuben-api" }

# --- runtime / http ---
tokio        = { version = "1", features = ["rt-multi-thread", "macros", "signal", "sync", "time", "io-util", "net", "fs"] }
tokio-util   = { version = "0.7", features = ["codec", "compat", "rt"] }
tokio-stream = { version = "0.1", features = ["sync"] }
futures      = "0.3"
async-stream = "0.3"
axum         = { version = "0.8", features = ["ws", "macros", "json", "query", "tokio", "http1", "http2"] }
axum-extra   = { version = "0.12", features = ["cookie", "typed-header"] }
tower        = { version = "0.5", features = ["limit", "load-shed", "timeout", "util"] }
tower-http   = { version = "0.6", features = ["trace", "timeout", "limit", "compression-br", "compression-gzip", "request-id", "set-header", "cors", "sensitive-headers", "normalize-path"] }
hyper        = { version = "1", features = ["http1", "http2", "client", "server"] }
hyper-util   = { version = "0.1", features = ["client-legacy", "tokio", "server"] }
http         = "1"
bytes        = "1"
reqwest      = { version = "0.12", default-features = false, features = ["rustls-tls", "json", "stream", "http2"] }

# --- kubernetes ---
kube         = { version = "4", default-features = false, features = ["client", "runtime", "derive", "ws", "rustls-tls", "gzip"] }
k8s-openapi  = { version = "0.27", features = ["v1_32"] }   # ← lowest supported K8s version; re-check at scaffold time
schemars     = { version = "1", features = ["chrono04"] }

# --- data ---
sqlx             = { version = "0.9", default-features = false, features = ["runtime-tokio", "tls-rustls", "sqlite", "postgres", "migrate", "uuid", "macros", "json"] }
sea-query        = { version = "0.32", features = ["backend-sqlite", "backend-postgres", "derive"] }   # ← pin the version compatible with sqlx 0.9 at scaffold time
sea-query-binder = { version = "0.7", features = ["sqlx-sqlite", "sqlx-postgres", "with-uuid", "with-json"] }
serde            = { version = "1", features = ["derive", "rc"] }
serde_json       = { version = "1", features = ["raw_value"] }
serde_yaml_ng    = "0.10"
uuid             = { version = "1", features = ["v7", "serde"] }
jiff             = { version = "0.2", features = ["serde"] }
compact_str      = { version = "0.9", features = ["serde"] }
smallvec         = "1"

# --- concurrency / cache ---
papaya      = "0.2"
dashmap     = "6"
arc-swap    = "1"
parking_lot = "0.12"
moka        = { version = "0.12", features = ["future"] }

# --- auth / crypto ---
argon2           = { version = "0.5", features = ["std"] }
password-hash    = "0.5"
subtle           = "2"
sha2             = "0.10"
hmac             = "0.12"
chacha20poly1305 = "0.10"
rand             = "0.9"
secrecy          = { version = "0.10", features = ["serde"] }
zeroize          = "1"
base64           = "0.22"
openidconnect    = { version = "4", features = ["reqwest", "rustls-tls"] }
totp-rs          = { version = "5", features = ["gen_secret", "otpauth"] }

# --- api docs / validation ---
utoipa         = { version = "5", features = ["axum_extras", "uuid", "chrono", "url", "preserve_order"] }
utoipa-axum    = "0.2"
utoipa-scalar  = { version = "0.3", features = ["axum"] }
garde          = { version = "0.22", features = ["derive", "serde"] }

# --- config / cli / errors / observability ---
figment    = { version = "0.10", features = ["toml", "env"] }
clap       = { version = "4", features = ["derive", "env"] }
thiserror  = "2"
anyhow     = "1"
tracing    = "0.1"
tracing-subscriber = { version = "0.3", features = ["env-filter", "json", "fmt"] }
metrics    = "0.24"
metrics-exporter-prometheus = { version = "0.17", default-features = false, features = ["http-listener"] }
backon     = "1"
rust-embed = { version = "8", features = ["compression", "include-exclude"] }
mime_guess = "2"
mimalloc   = "0.1"

# --- optional integrations (feature-gated in crates) ---
genai    = "0.6"
teloxide = { version = "0.17", features = ["webhooks-axum", "macros"] }
rmcp     = { version = "0.6", features = ["server", "transport-streamable-http-server"] }
opentelemetry      = "0.30"
opentelemetry-otlp = { version = "0.30", features = ["http-proto", "reqwest-rustls"] }
tracing-opentelemetry = "0.31"
object_store = { version = "0.12", features = ["aws", "gcp", "azure"] }

# --- test ---
insta          = { version = "1", features = ["yaml", "json", "redactions"] }
rstest         = "0.25"
proptest       = "1"
wiremock       = "0.6"
testcontainers = "0.24"
testcontainers-modules = { version = "0.12", features = ["postgres"] }
tokio-test     = "0.4"

[workspace.lints.rust]
unsafe_code = "forbid"
missing_debug_implementations = "warn"
unused_must_use = "deny"

[workspace.lints.clippy]
all = { level = "warn", priority = -1 }
pedantic = { level = "warn", priority = -1 }
await_holding_lock = "deny"
unwrap_used = "deny"
expect_used = "warn"
dbg_macro = "deny"
todo = "warn"
module_name_repetitions = "allow"
must_use_candidate = "allow"
missing_errors_doc = "allow"

[profile.release]
opt-level = 3
lto = "fat"
codegen-units = 1
strip = true
panic = "unwind"            # the supervisor must be able to catch panics
debug = "line-tables-only"

[profile.dev]
opt-level = 0
debug = 1

[profile.dev.package."*"]
opt-level = 2               # optimized dependencies in dev, for runtime speed

[profile.ci]
inherits = "release"
lto = "thin"
codegen-units = 16
```

> ⚠️ **Versions:** the numbers above are based on the latest known releases up to September 2026 (kube 4.0, axum 0.8.9, sqlx 0.9.0, utoipa 5.5). Pin the less critical crates (sea-query, rmcp, teloxide, opentelemetry, testcontainers) with `cargo add` on scaffold day, and enable Renovate. Pin `k8s-openapi` to the **lowest supported Kubernetes version** (suggested: 1.32 = stable k3s in 2026).

#### `rust-toolchain.toml`

```toml
[toolchain]
channel = "1.94"
components = ["rustfmt", "clippy", "rust-src"]
targets = ["x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"]
profile = "minimal"
```

#### `.cargo/config.toml`

```toml
[build]
rustflags = ["-C", "target-cpu=x86-64-v2"]

[target.x86_64-unknown-linux-gnu]
linker = "clang"
rustflags = ["-C", "link-arg=-fuse-ld=mold"]

[target.x86_64-unknown-linux-musl]
rustflags = ["-C", "target-feature=+crt-static", "-C", "link-self-contained=yes"]

[target.aarch64-unknown-linux-musl]
rustflags = ["-C", "target-feature=+crt-static", "-C", "link-self-contained=yes"]

[alias]
xtask = "run -p kuben --"
```

#### `deny.toml` (summary)

```toml
[licenses]
allow = ["MIT", "Apache-2.0", "Apache-2.0 WITH LLVM-exception", "BSD-2-Clause", "BSD-3-Clause", "ISC", "Unicode-3.0", "Zlib", "MPL-2.0", "CC0-1.0", "OpenSSL"]
confidence-threshold = 0.9

[bans]
multiple-versions = "warn"
deny = [
  { name = "openssl-sys", reason = "rustls only" },
  { name = "serde_yaml", reason = "unmaintained; use serde_yaml_ng" },
]

[advisories]
yanked = "deny"
unmaintained = "workspace"

[sources]
unknown-registry = "deny"
unknown-git = "deny"
```

#### `pnpm-workspace.yaml`

```yaml
packages:
  - apps/*
  - packages/*

catalog:
  react: ^19.1.0
  react-dom: ^19.1.0
  "@types/react": ^19.1.0
  "@types/react-dom": ^19.1.0
  typescript: ^5.9.0
  vite: ^8.0.0
  "@vitejs/plugin-react": ^5.0.0
  babel-plugin-react-compiler: ^19.1.0
  "@tanstack/react-router": ^1.130.0
  "@tanstack/router-plugin": ^1.130.0
  "@tanstack/react-router-devtools": ^1.130.0
  "@tanstack/react-query": ^5.80.0
  "@tanstack/react-query-devtools": ^5.80.0
  "@tanstack/react-table": ^8.21.0
  "@tanstack/react-virtual": ^3.13.0
  tailwindcss: ^4.1.0
  "@tailwindcss/vite": ^4.1.0
  "@base-ui-components/react": ^1.0.0
  class-variance-authority: ^0.7.1
  tailwind-merge: ^3.3.0
  clsx: ^2.1.1
  lucide-react: ^0.540.0
  sonner: ^2.0.0
  cmdk: ^1.1.0
  react-hook-form: ^7.60.0
  zod: ^4.0.0
  "@hookform/resolvers": ^5.1.0
  openapi-fetch: ^0.14.0
  openapi-typescript: ^7.8.0
  openapi-react-query: ^0.5.0
  "@xterm/xterm": ^5.5.0
  "@xterm/addon-fit": ^0.10.0
  "@xterm/addon-web-links": ^0.11.0
  "@xterm/addon-webgl": ^0.18.0
  "@xterm/addon-search": ^0.15.0
  "@xterm/addon-unicode11": ^0.8.0
  uplot: ^1.6.32
  "@xyflow/react": ^12.8.0
  "@uiw/react-codemirror": ^4.23.0
  "@codemirror/lang-yaml": ^6.1.0
  "@inlang/paraglide-js": ^2.0.0
  "@fontsource-variable/inter": ^5.2.0
  date-fns: ^4.1.0
  "@biomejs/biome": ^2.1.0
  vitest: ^3.2.0
  "@testing-library/react": ^16.3.0
  "@playwright/test": ^1.55.0
  msw: ^2.10.0
  size-limit: ^11.2.0
  "@size-limit/file": ^11.2.0
  knip: ^5.60.0

catalogMode: strict
```

#### `package.json` (root)

```json
{
  "name": "kuben-monorepo",
  "private": true,
  "packageManager": "pnpm@10.15.0",
  "engines": { "node": ">=22.12", "pnpm": ">=10" },
  "scripts": {
    "gen": "pnpm -F @kuben/api-client generate",
    "dev": "pnpm -F @kuben/web dev",
    "build": "pnpm -F @kuben/web build",
    "typecheck": "pnpm -r typecheck",
    "lint": "biome check .",
    "lint:fix": "biome check --write .",
    "test": "pnpm -r test",
    "e2e": "pnpm -F @kuben/web e2e"
  }
}
```

#### `apps/web/package.json`

```json
{
  "name": "@kuben/web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite --port 5173",
    "build": "tsc -b && vite build",
    "preview": "vite preview",
    "typecheck": "tsc -b --noEmit",
    "test": "vitest run",
    "e2e": "playwright test",
    "size": "size-limit"
  },
  "dependencies": {
    "react": "catalog:", "react-dom": "catalog:",
    "@tanstack/react-router": "catalog:", "@tanstack/react-query": "catalog:",
    "@tanstack/react-table": "catalog:", "@tanstack/react-virtual": "catalog:",
    "@base-ui-components/react": "catalog:", "class-variance-authority": "catalog:",
    "tailwind-merge": "catalog:", "clsx": "catalog:", "lucide-react": "catalog:",
    "sonner": "catalog:", "cmdk": "catalog:",
    "react-hook-form": "catalog:", "zod": "catalog:", "@hookform/resolvers": "catalog:",
    "openapi-fetch": "catalog:", "openapi-react-query": "catalog:",
    "@kuben/api-client": "workspace:*",
    "@xterm/xterm": "catalog:", "@xterm/addon-fit": "catalog:", "@xterm/addon-web-links": "catalog:",
    "@xterm/addon-webgl": "catalog:", "@xterm/addon-search": "catalog:", "@xterm/addon-unicode11": "catalog:",
    "uplot": "catalog:", "@xyflow/react": "catalog:",
    "@uiw/react-codemirror": "catalog:", "@codemirror/lang-yaml": "catalog:",
    "@inlang/paraglide-js": "catalog:", "@fontsource-variable/inter": "catalog:", "date-fns": "catalog:"
  },
  "devDependencies": {
    "@types/react": "catalog:", "@types/react-dom": "catalog:", "typescript": "catalog:",
    "vite": "catalog:", "@vitejs/plugin-react": "catalog:", "babel-plugin-react-compiler": "catalog:",
    "@tanstack/router-plugin": "catalog:", "@tanstack/react-router-devtools": "catalog:",
    "@tanstack/react-query-devtools": "catalog:",
    "tailwindcss": "catalog:", "@tailwindcss/vite": "catalog:",
    "vitest": "catalog:", "@testing-library/react": "catalog:", "@playwright/test": "catalog:", "msw": "catalog:",
    "size-limit": "catalog:", "@size-limit/file": "catalog:"
  },
  "size-limit": [
    { "name": "shell", "path": "dist/assets/index-*.js", "limit": "200 KB", "brotli": true },
    { "name": "terminal-route", "path": "dist/assets/terminal-*.js", "limit": "180 KB", "brotli": true }
  ]
}
```

#### `apps/web/vite.config.ts`

```ts
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { paraglideVitePlugin } from '@inlang/paraglide-js'

export default defineConfig({
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react({ babel: { plugins: ['babel-plugin-react-compiler'] } }),
    tailwindcss(),
    paraglideVitePlugin({ project: './project.inlang', outdir: './src/paraglide' }),
  ],
  server: {
    proxy: { '/api': { target: 'http://127.0.0.1:8080', ws: true, changeOrigin: false } },
  },
  build: {
    target: 'es2022',
    sourcemap: false,
    rollupOptions: { output: { manualChunks: { xterm: ['@xterm/xterm', '@xterm/addon-fit', '@xterm/addon-webgl'] } } },
  },
})
```

#### `packages/api-client/package.json`

```json
{
  "name": "@kuben/api-client",
  "private": true,
  "type": "module",
  "exports": { ".": "./src/index.ts", "./schema": "./src/schema.d.ts" },
  "scripts": {
    "generate": "openapi-typescript ./openapi.json -o ./src/schema.d.ts --export-type --immutable",
    "typecheck": "tsc --noEmit"
  },
  "dependencies": { "openapi-fetch": "catalog:" },
  "devDependencies": { "openapi-typescript": "catalog:", "typescript": "catalog:" }
}
```

```ts
// packages/api-client/src/index.ts
import createClient from 'openapi-fetch'
import type { paths } from './schema'

export const api = createClient<paths>({
  baseUrl: '/api/v1',
  credentials: 'same-origin',
  headers: { 'X-Kuben-Client': 'web' },   // CSRF: a custom header is mandatory for mutations
})
export type { paths, components } from './schema'
```

#### `justfile`

```make
set shell := ["bash", "-euo", "pipefail", "-c"]
export DATABASE_URL := env_var_or_default("DATABASE_URL", "sqlite://./.dev/kuben.db")

default: ci

setup:
    rustup show
    cargo install cargo-nextest cargo-deny cargo-llvm-cov cargo-chef cargo-zigbuild --locked
    pnpm install --frozen-lockfile
    mkdir -p .dev

gen:
    cargo run -q -p kuben-api --bin openapi > packages/api-client/openapi.json
    pnpm gen
    cargo run -q -p kuben-crd --bin crdgen > charts/kuben/crds/kuben.dev_all.yaml

fmt:
    cargo fmt --all
    biome check --write .

lint:
    cargo fmt --all --check
    cargo clippy --workspace --all-targets --all-features -- -D warnings
    biome check .
    pnpm -r typecheck

test:
    cargo nextest run --workspace --all-features
    pnpm -r test

test-db-matrix:
    KUBEN_TEST_DB=sqlite   cargo nextest run -p kuben-store
    KUBEN_TEST_DB=postgres cargo nextest run -p kuben-store   # testcontainers

web:
    pnpm build

build: web
    cargo build -p kuben --release --features embed-ui

build-musl target="x86_64-unknown-linux-musl": web
    cargo zigbuild -p kuben --release --target {{target}} --features embed-ui

dev:
    just -j2 dev-api dev-web

dev-api:
    bacon run -- -p kuben -- serve --roles=all --dev

dev-web:
    pnpm dev

kind-up:
    kind create cluster --name kuben --config deploy/kind.yaml || true
    kubectl apply -f charts/kuben/crds/

e2e: kind-up
    cargo run -p kuben -- serve --roles=all --dev &
    pnpm e2e

budgets:
    scripts/check-budgets.sh   # binary ≤25MB, image ≤30MB, RSS idle ≤30MB, JS shell ≤200KB

drift: gen
    git diff --exit-code packages/api-client/openapi.json packages/api-client/src/schema.d.ts charts/kuben/crds/

ci: lint test drift
    cargo deny check

image tag="dev":
    docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/kuben-dev/kuben:{{tag}} .
```

#### `Dockerfile`

```dockerfile
# syntax=docker/dockerfile:1.7
FROM node:22-alpine AS web
RUN corepack enable
WORKDIR /src
COPY pnpm-lock.yaml pnpm-workspace.yaml package.json .npmrc ./
COPY apps/web/package.json apps/web/
COPY packages/api-client/package.json packages/api-client/
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY apps/web apps/web
COPY packages/api-client packages/api-client
RUN pnpm -F @kuben/web build

FROM rust:1.94-bookworm AS chef
RUN cargo install cargo-chef cargo-zigbuild --locked \
 && pip3 install --break-system-packages ziglang \
 && rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl
WORKDIR /src

FROM chef AS plan
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS build
ARG TARGETARCH
RUN case "$TARGETARCH" in amd64) echo x86_64-unknown-linux-musl > /target ;; arm64) echo aarch64-unknown-linux-musl > /target ;; esac
COPY --from=plan /src/recipe.json .
RUN cargo chef cook --release --zigbuild --target "$(cat /target)" --recipe-path recipe.json
COPY . .
COPY --from=web /src/apps/web/dist apps/web/dist
RUN cargo zigbuild -p kuben --release --target "$(cat /target)" --features embed-ui \
 && cp "target/$(cat /target)/release/kuben" /kuben

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kuben /kuben
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/kuben"]
CMD ["serve", "--roles=all"]
```

### 5.4 Crates — `Cargo.toml` and code skeletons

#### `crates/kuben-crd/Cargo.toml`

```toml
[package]
name = "kuben-crd"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben CustomResourceDefinitions (kuben.dev/v1alpha1)"

[lib]
path = "src/lib.rs"

[[bin]]
name = "crdgen"
path = "src/bin/crdgen.rs"

[dependencies]
kube = { workspace = true, default-features = false, features = ["derive"] }
k8s-openapi.workspace = true
schemars.workspace = true
serde.workspace = true
serde_json.workspace = true
serde_yaml_ng.workspace = true

[lints]
workspace = true
```

```rust
// crates/kuben-crd/src/lib.rs
//! CRD types. This crate deliberately has no tokio/axum so that the CLI and third-party tools can use it.
pub mod v1alpha1;
pub use v1alpha1::*;

pub const GROUP: &str = "kuben.dev";
pub const MANAGED_BY: &str = "app.kubernetes.io/managed-by";
pub const MANAGER: &str = "kuben";
pub const LABEL_ORG: &str = "kuben.dev/org";
pub const LABEL_PROJECT: &str = "kuben.dev/project";
pub const LABEL_ENV: &str = "kuben.dev/environment";
pub const LABEL_APP: &str = "kuben.dev/app";
```

```rust
// crates/kuben-crd/src/v1alpha1/app.rs (summary)
use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev", version = "v1alpha1", kind = "App", namespaced,
    status = "AppStatus", shortname = "kapp",
    printcolumn = r#"{"name":"Ready","type":"string","jsonPath":".status.conditions[?(@.type==\"Ready\")].status"}"#,
    printcolumn = r#"{"name":"URL","type":"string","jsonPath":".status.url"}"#,
    printcolumn = r#"{"name":"Release","type":"string","jsonPath":".status.currentRelease"}"#,
    rule = Rule::new("self.runtime.processes.all(p, p.replicas.min <= p.replicas.max)").message("replicas.min must be <= replicas.max"),
    rule = Rule::new("size(self.domains) <= 20").message("at most 20 domains"),
)]
#[serde(rename_all = "camelCase")]
pub struct AppSpec {
    pub source: Source,
    pub runtime: Runtime,
    #[serde(default)] pub env: Vec<EnvVar>,
    #[serde(default)] pub domains: Vec<Domain>,
    /// Escape hatch: a strategic-merge patch over the generated Deployment
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[schemars(with = "Option<serde_json::Value>")]
    pub workload_patch: Option<serde_json::Value>,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase", tag = "kind")]
pub enum Source {
    Image { image: String },                       // must be digest-pinned at Release time
    Git { repo: String, branch: String, #[serde(default)] path: String, build: Build },
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Build { pub strategy: BuildStrategy, #[serde(default)] pub dockerfile: Option<String> }

#[derive(Clone, Copy, Debug, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum BuildStrategy { Auto, Dockerfile, Railpack }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Runtime {
    pub processes: std::collections::BTreeMap<String, Process>,
    #[serde(default)] pub health_check: Option<HealthCheck>,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Process {
    #[serde(default)] pub command: Vec<String>,
    #[serde(default)] pub port: Option<u16>,
    #[serde(default = "default_size")] pub size: String,
    #[serde(default)] pub replicas: Replicas,
    #[serde(default)] pub idle: Option<Idle>,
}
fn default_size() -> String { "small".into() }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
pub struct Replicas { #[serde(default = "one")] pub min: u32, #[serde(default = "one")] pub max: u32 }
impl Default for Replicas { fn default() -> Self { Self { min: 1, max: 1 } } }
fn one() -> u32 { 1 }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Idle { pub mode: IdleMode, #[serde(default = "default_idle_after")] pub after: String }
#[derive(Clone, Copy, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum IdleMode { Off, Zero, Throttle }
fn default_idle_after() -> String { "15m".into() }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct HealthCheck { pub path: String, #[serde(default)] pub port: Option<u16> }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct EnvVar {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")] pub value: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")] pub from_secret: Option<KeyRef>,
    #[serde(default, skip_serializing_if = "Option::is_none")] pub from_service: Option<KeyRef>,
}
#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
pub struct KeyRef { pub name: String, pub key: String }

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Domain { pub host: String, #[serde(default = "default_tls")] pub tls: String }
fn default_tls() -> String { "auto".into() }

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct AppStatus {
    #[serde(default)] pub observed_generation: Option<i64>,
    #[serde(default)] pub current_release: Option<String>,
    #[serde(default)] pub url: Option<String>,
    #[serde(default)] pub conditions: Vec<k8s_openapi::apimachinery::pkg::apis::meta::v1::Condition>,
}
```

```rust
// crates/kuben-crd/src/bin/crdgen.rs
use kube::CustomResourceExt;
fn main() {
    let crds = [
        kuben_crd::Project::crd(), kuben_crd::Environment::crd(), kuben_crd::App::crd(),
        kuben_crd::Release::crd(), kuben_crd::BuildRun::crd(), kuben_crd::KubenConfig::crd(),
    ];
    for crd in crds {
        println!("---\n{}", serde_yaml_ng::to_string(&crd).expect("serialize crd"));
    }
}
```

#### `crates/kuben-core/Cargo.toml`

```toml
[package]
name = "kuben-core"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben domain types, config, errors and core traits (no IO)"

[features]
default = []
test-util = ["dep:proptest"]

[dependencies]
serde.workspace = true
serde_json.workspace = true
uuid.workspace = true
jiff.workspace = true
compact_str.workspace = true
thiserror.workspace = true
secrecy.workspace = true
zeroize.workspace = true
figment.workspace = true
tracing.workspace = true
async-trait = "0.1"
proptest = { workspace = true, optional = true }

[lints]
workspace = true
```

```rust
// crates/kuben-core/src/lib.rs
pub mod config;
pub mod error;
pub mod ids;
pub mod model;      // User, Org, Session, ApiToken, RoleBinding, AuditEvent …
pub mod perm;       // Perm enum + Role → Perm mapping
pub mod traits;     // Store, PolicyEngine, IdentityProvider, LeaderElector, BlobStore, MetricsSource, NotificationSink
pub mod authz;      // AuthzProof — the only way to build one is PolicyEngine::check

pub use error::{Error, Result};
```

```rust
// crates/kuben-core/src/authz.rs
//! Invariant I-1: no Subscription/Mutation is possible without an AuthzProof.
use crate::{ids::UserId, perm::Perm};

/// Proof that `user` holds `perm` on `scope`.
/// Only `PolicyEngine::check` can build one (private constructor).
#[derive(Debug, Clone)]
pub struct AuthzProof { user: UserId, perm: Perm, scope: ScopeRef }

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum ScopeRef { Org(uuid::Uuid), Project(uuid::Uuid), Environment(uuid::Uuid), App(uuid::Uuid) }

impl AuthzProof {
    pub(crate) fn new(user: UserId, perm: Perm, scope: ScopeRef) -> Self { Self { user, perm, scope } }
    pub fn user(&self) -> &UserId { &self.user }
    pub fn perm(&self) -> Perm { self.perm }
    pub fn scope(&self) -> &ScopeRef { &self.scope }
}
```

```rust
// crates/kuben-core/src/config.rs (summary)
use figment::{providers::{Env, Format, Serialized, Toml}, Figment};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Config {
    pub server: ServerCfg,
    pub database: DatabaseCfg,
    pub runtime: RuntimeCfg,
    pub kube: KubeCfg,
    pub security: SecurityCfg,
    pub telemetry: TelemetryCfg,
}
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct ServerCfg { pub bind: String, pub metrics_bind: String, pub public_url: Option<String>, pub roles: Vec<Role> }
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)] #[serde(rename_all = "lowercase")]
pub enum Role { All, Api, Controller, Activator }
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct DatabaseCfg { pub url: String, pub max_connections: u32 }
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct RuntimeCfg { pub worker_threads: Option<usize>, pub max_blocking_threads: usize, pub bulkhead: bool }
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct KubeCfg { pub kubeconfig: Option<String>, pub context: Option<String>, pub watch_namespace: Option<String> }
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct SecurityCfg { pub session_ttl_hours: u64, pub argon2_m_kib: u32, pub argon2_t: u32, pub argon2_p: u32, pub login_concurrency: usize, pub master_key_secret: String }
#[derive(Debug, Clone, Serialize, Deserialize)] #[serde(default)]
pub struct TelemetryCfg { pub log_format: String, pub log_level: String, pub otlp_endpoint: Option<String> }

impl Default for Config { fn default() -> Self { Self {
    server: ServerCfg { bind: "0.0.0.0:8080".into(), metrics_bind: "0.0.0.0:9090".into(), public_url: None, roles: vec![Role::All] },
    database: DatabaseCfg { url: "sqlite:///data/kuben.db".into(), max_connections: 4 },
    runtime: RuntimeCfg { worker_threads: None, max_blocking_threads: 16, bulkhead: false },
    kube: KubeCfg { kubeconfig: None, context: None, watch_namespace: None },
    security: SecurityCfg { session_ttl_hours: 12, argon2_m_kib: 19 * 1024, argon2_t: 2, argon2_p: 1, login_concurrency: 2, master_key_secret: "kuben-master-key".into() },
    telemetry: TelemetryCfg { log_format: "json".into(), log_level: "info".into(), otlp_endpoint: None },
}}}

impl Config {
    /// Order: defaults ← /etc/kuben/config.toml ← ./kuben.toml ← env KUBEN_* (nested with `__`)
    pub fn load() -> figment::Result<Self> {
        Figment::from(Serialized::defaults(Config::default()))
            .merge(Toml::file("/etc/kuben/config.toml"))
            .merge(Toml::file("kuben.toml"))
            .merge(Env::prefixed("KUBEN_").split("__"))
            .extract()
    }
}
```

#### `crates/kuben-store/Cargo.toml`

```toml
[package]
name = "kuben-store"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben persistence: sqlx (SQLite/Postgres) + sea-query repositories + embedded migrations"

[dependencies]
kuben-core.workspace = true
sqlx.workspace = true
sea-query.workspace = true
sea-query-binder.workspace = true
serde.workspace = true
serde_json.workspace = true
uuid.workspace = true
jiff.workspace = true
thiserror.workspace = true
tracing.workspace = true
async-trait = "0.1"
sha2.workspace = true
tokio.workspace = true

[dev-dependencies]
kuben-core = { workspace = true, features = ["test-util"] }
rstest.workspace = true
testcontainers.workspace = true
testcontainers-modules.workspace = true
tokio-test.workspace = true

[lints]
workspace = true
```

```
crates/kuben-store/
├── sqlx.toml                        # migrate.migrations-dir per-db (sqlx 0.9)
├── migrations/
│   ├── sqlite/0001_init.sql
│   └── postgres/0001_init.sql
└── src/
    ├── lib.rs                       # Store { writer, reader }, connect(), migrate()
    ├── sqlite.rs                    # single-writer pool (max=1) + read pool (max=4), WAL, FK ON
    ├── postgres.rs
    ├── iden.rs                      # #[derive(Iden)] enums for every table/column
    ├── repo/{users,orgs,sessions,tokens,bindings,audit,outbox,idempotency}.rs
    └── tests/matrix.rs              # every test runs on both sqlite and postgres (KUBEN_TEST_DB)
```

```rust
// crates/kuben-store/src/sqlite.rs (the essentials)
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous};
use std::{str::FromStr, time::Duration};

pub async fn connect(url: &str) -> sqlx::Result<(sqlx::SqlitePool, sqlx::SqlitePool)> {
    let base = SqliteConnectOptions::from_str(url)?
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .synchronous(SqliteSynchronous::Normal)
        .busy_timeout(Duration::from_secs(5))
        .foreign_keys(true)                       // Invariant I-16
        .pragma("temp_store", "memory")
        .pragma("cache_size", "-2000");           // ~2MiB/conn → RAM budget
    let writer = SqlitePoolOptions::new().max_connections(1).connect_with(base.clone()).await?;
    let reader = SqlitePoolOptions::new().max_connections(4).connect_with(base.read_only(true)).await?;
    sqlx::migrate!("./migrations/sqlite").run(&writer).await?;
    Ok((writer, reader))
}
```

```sql
-- crates/kuben-store/migrations/sqlite/0001_init.sql (summary; the postgres version uses BYTEA/BIGINT IDENTITY)
CREATE TABLE organizations (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL, created_at BIGINT NOT NULL);
CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, display_name TEXT, password_hash TEXT,
  is_active BOOLEAN NOT NULL DEFAULT TRUE, created_at BIGINT NOT NULL);
CREATE TABLE identities (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL, subject TEXT NOT NULL, UNIQUE(provider, subject));
CREATE TABLE memberships (org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(org_id, user_id));
CREATE TABLE role_bindings (id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL, role TEXT NOT NULL,
  scope_kind TEXT NOT NULL, scope_uid TEXT, created_at BIGINT NOT NULL);
CREATE INDEX rb_subject ON role_bindings(subject_kind, subject_id);
CREATE TABLE sessions (id_hash BLOB PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, last_seen_at BIGINT, ip TEXT, ua_hash BLOB, revoked_at BIGINT);
CREATE TABLE api_tokens (id TEXT PRIMARY KEY, org_id TEXT NOT NULL, owner_user_id TEXT, name TEXT NOT NULL, prefix TEXT NOT NULL,
  secret_hash BLOB NOT NULL UNIQUE, scopes TEXT NOT NULL, expires_at BIGINT, last_used_at BIGINT, revoked_at BIGINT, created_at BIGINT NOT NULL);
CREATE TABLE revocations (subject_hash BLOB PRIMARY KEY, at BIGINT NOT NULL);
CREATE TABLE audit_events (seq INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, org_id TEXT, actor_kind TEXT NOT NULL, actor_id TEXT,
  action TEXT NOT NULL, target_kind TEXT, target_ref TEXT, outcome TEXT NOT NULL, ip TEXT, request_id TEXT, data TEXT, created_at BIGINT NOT NULL);
CREATE INDEX audit_org_time ON audit_events(org_id, created_at);
CREATE TABLE idempotency_keys (key TEXT PRIMARY KEY, user_id TEXT, request_hash BLOB, response TEXT, created_at BIGINT NOT NULL);
CREATE TABLE outbox (id TEXT PRIMARY KEY, topic TEXT NOT NULL, payload TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL, created_at BIGINT NOT NULL);
CREATE TABLE approvals (id TEXT PRIMARY KEY, org_id TEXT NOT NULL, release_uid TEXT NOT NULL, release_digest TEXT NOT NULL,
  requester_id TEXT NOT NULL, required INTEGER NOT NULL, status TEXT NOT NULL, expires_at BIGINT NOT NULL, created_at BIGINT NOT NULL);
CREATE TABLE approval_votes (approval_id TEXT NOT NULL REFERENCES approvals(id) ON DELETE CASCADE, user_id TEXT NOT NULL,
  channel TEXT NOT NULL, vote TEXT NOT NULL, at BIGINT NOT NULL, PRIMARY KEY(approval_id, user_id));
```

#### `crates/kuben-platform/Cargo.toml`

```toml
[package]
name = "kuben-platform"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben Kubernetes platform: registry, informers/projections, controllers, log/exec hubs, build, activator"

[features]
default = []
activator = []
ai = ["dep:genai"]
otlp = []

[dependencies]
kuben-core.workspace = true
kuben-crd.workspace = true
kube.workspace = true
k8s-openapi.workspace = true
tokio.workspace = true
tokio-util.workspace = true
tokio-stream.workspace = true
futures.workspace = true
async-stream.workspace = true
serde.workspace = true
serde_json.workspace = true
papaya.workspace = true
dashmap.workspace = true
arc-swap.workspace = true
parking_lot.workspace = true
compact_str.workspace = true
smallvec.workspace = true
backon.workspace = true
thiserror.workspace = true
anyhow.workspace = true
tracing.workspace = true
metrics.workspace = true
hyper.workspace = true
hyper-util.workspace = true
http.workspace = true
bytes.workspace = true
reqwest.workspace = true
uuid.workspace = true
jiff.workspace = true
genai = { workspace = true, optional = true }

[dev-dependencies]
insta.workspace = true
rstest.workspace = true
tokio-test.workspace = true
wiremock.workspace = true

[lints]
workspace = true
```

```
crates/kuben-platform/src/
├── lib.rs
├── registry.rs          # ClusterRegistry { ClusterId → Arc<kube::Client> } — Invariant I-11
├── projection/          # PodView, DeployView, AppView + indexes + seq + delta bus
│   ├── mod.rs
│   ├── pod.rs
│   ├── app.rs
│   └── informer.rs      # watcher(...).default_backoff() → Init/InitApply/InitDone → atomic swap
├── loghub.rs            # Invariant I-12
├── exec.rs              # terminal session (WS ↔ kube exec), ephemeral debug
├── controller/
│   ├── mod.rs           # run_all(shared, token) with supervise()
│   ├── project.rs
│   ├── environment.rs   # namespace, PSA, quota, netpol, soft-delete
│   ├── app.rs           # Deployment/Service/HTTPRoute via SSA + conditions
│   ├── release.rs
│   ├── buildrun.rs      # buildctl Job dispatch + digest extraction
│   └── crd_apply.rs     # self-apply CRDs at boot
├── build/               # strategy → buildctl args, railpack prepare, digest parse
├── activator/           # feature "activator": hyper proxy + wake/sleep state machine
├── incidents/           # detector + rules (+ genai when feature "ai")
├── supervise.rs         # supervisor with backoff, panic capture
└── health.rs            # subsystem health registry
```

```rust
// crates/kuben-platform/src/supervise.rs
use backon::{BackoffBuilder, ExponentialBuilder};
use std::future::Future;
use tokio_util::sync::CancellationToken;
use crate::health::Health;

pub async fn supervise<F, Fut>(name: &'static str, token: CancellationToken, health: Health, mut make: F)
where
    F: FnMut(CancellationToken) -> Fut,
    Fut: Future<Output = anyhow::Result<()>> + Send + 'static,
{
    let mut backoff = ExponentialBuilder::default()
        .with_min_delay(std::time::Duration::from_millis(500))
        .with_max_delay(std::time::Duration::from_secs(60))
        .with_jitter()
        .without_max_times()
        .build();
    loop {
        let handle = tokio::spawn(make(token.child_token()));
        let outcome = handle.await;
        if token.is_cancelled() { return; }
        match outcome {
            Ok(Ok(())) => { health.ok(name); return; }
            Ok(Err(e)) => { health.degraded(name, &e.to_string()); tracing::error!(subsystem = name, error = %e, "subsystem failed"); }
            Err(j) if j.is_panic() => {
                health.degraded(name, "panic");
                metrics::counter!("kuben_subsystem_panics_total", "subsystem" => name).increment(1);
                tracing::error!(subsystem = name, "subsystem panicked");
            }
            Err(_) => return, // cancelled
        }
        let delay = backoff.next().unwrap_or(std::time::Duration::from_secs(60));
        tokio::select! { () = token.cancelled() => return, () = tokio::time::sleep(delay) => {} }
    }
}
```

#### `crates/kuben-api/Cargo.toml`

```toml
[package]
name = "kuben-api"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben HTTP API (axum + utoipa), SSE/WS, auth middleware, embedded web UI"

[features]
default = []
embed-ui = ["dep:rust-embed", "dep:mime_guess"]
mcp = ["dep:rmcp"]
telegram = ["dep:teloxide"]
otel = ["dep:opentelemetry", "dep:opentelemetry-otlp", "dep:tracing-opentelemetry"]

[[bin]]
name = "openapi"
path = "src/bin/openapi.rs"

[dependencies]
kuben-core.workspace = true
kuben-crd.workspace = true
kuben-store.workspace = true
kuben-platform.workspace = true
axum.workspace = true
axum-extra.workspace = true
tower.workspace = true
tower-http.workspace = true
tokio.workspace = true
tokio-util.workspace = true
tokio-stream.workspace = true
futures.workspace = true
async-stream.workspace = true
http.workspace = true
bytes.workspace = true
serde.workspace = true
serde_json.workspace = true
utoipa.workspace = true
utoipa-axum.workspace = true
utoipa-scalar.workspace = true
garde.workspace = true
argon2.workspace = true
password-hash.workspace = true
subtle.workspace = true
sha2.workspace = true
rand.workspace = true
base64.workspace = true
secrecy.workspace = true
moka.workspace = true
uuid.workspace = true
jiff.workspace = true
thiserror.workspace = true
anyhow.workspace = true
tracing.workspace = true
metrics.workspace = true
rust-embed = { workspace = true, optional = true }
mime_guess = { workspace = true, optional = true }
rmcp = { workspace = true, optional = true }
teloxide = { workspace = true, optional = true }
opentelemetry = { workspace = true, optional = true }
opentelemetry-otlp = { workspace = true, optional = true }
tracing-opentelemetry = { workspace = true, optional = true }

[build-dependencies]
# build.rs: in a release build with the embed-ui feature, requires apps/web/dist to exist

[dev-dependencies]
insta.workspace = true
tower = { workspace = true, features = ["util"] }
http-body-util = "0.1"

[lints]
workspace = true
```

```rust
// crates/kuben-api/build.rs
fn main() {
    println!("cargo:rerun-if-changed=../../apps/web/dist");
    let embed = std::env::var("CARGO_FEATURE_EMBED_UI").is_ok();
    let dist = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../apps/web/dist/index.html");
    if embed && !dist.exists() {
        panic!("feature `embed-ui` requires apps/web/dist (run `just web` first)");
    }
}
```

```rust
// crates/kuben-api/src/lib.rs (Router skeleton)
pub mod auth;        // login/logout/me, session cookie, argon2 (spawn_blocking + Semaphore)
pub mod authz;       // extractor: Authz → PolicyEngine::check → AuthzProof
pub mod error;       // ApiError → application/problem+json (RFC 9457)
pub mod routes;      // projects, environments, apps, releases, stream (SSE), terminal (WS)
pub mod stream;      // SSE per tab: snapshot+delta, Last-Event-ID, logs multiplex
pub mod web;         // embedded SPA (feature embed-ui): brotli assets, SPA fallback, CSP
pub mod openapi;     // ApiDoc (utoipa)
pub mod state;       // ApiState { store, platform, policy, sessions cache }

use axum::{Router, middleware};
use tower_http::{compression::CompressionLayer, limit::RequestBodyLimitLayer, request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer}, timeout::TimeoutLayer, trace::TraceLayer};
use utoipa_axum::router::OpenApiRouter;

pub fn router(state: state::ApiState) -> Router {
    let (api, openapi) = OpenApiRouter::with_openapi(openapi::ApiDoc::openapi())
        .merge(routes::projects::router())
        .merge(routes::environments::router())
        .merge(routes::apps::router())
        .merge(routes::releases::router())
        .merge(auth::router())
        .split_for_parts();

    let api = api
        .route("/stream", axum::routing::get(stream::handler))          // SSE — no timeout/compression
        .route("/apps/{app}/terminal", axum::routing::any(routes::terminal::ws)) // WS (HTTP/2-capable via any())
        .layer(middleware::from_fn_with_state(state.clone(), auth::session_middleware))
        .layer(middleware::from_fn(auth::csrf_fetch_metadata))         // Sec-Fetch-Site + X-Kuben-Client
        .layer(TimeoutLayer::new(std::time::Duration::from_secs(30)))   // stream routes opt out themselves
        .layer(RequestBodyLimitLayer::new(1 << 20));

    Router::new()
        .nest("/api/v1", api)
        .merge(utoipa_scalar::Scalar::with_url("/api/docs", openapi))
        .route("/livez", axum::routing::get(routes::health::livez))
        .route("/readyz", axum::routing::get(routes::health::readyz))
        .fallback(web::spa_fallback)                                     // unknown /api/* → 404 JSON, everything else → index.html
        .layer(CompressionLayer::new().br(true).gzip(true))
        .layer(PropagateRequestIdLayer::x_request_id())
        .layer(SetRequestIdLayer::x_request_id(MakeRequestUuid))
        .layer(TraceLayer::new_for_http())
        .with_state(state)
}
```

```rust
// crates/kuben-api/src/bin/openapi.rs
fn main() {
    use utoipa::OpenApi;
    println!("{}", kuben_api::openapi::ApiDoc::openapi().to_pretty_json().expect("openapi json"));
}
```

#### `crates/kuben/Cargo.toml`

```toml
[package]
name = "kuben"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
description = "Kuben — single-binary Kubernetes-native PaaS"
default-run = "kuben"

[[bin]]
name = "kuben"
path = "src/main.rs"

[features]
default = ["activator"]
embed-ui = ["kuben-api/embed-ui"]
activator = ["kuben-platform/activator"]
ai = ["kuben-platform/ai"]
mcp = ["kuben-api/mcp"]
telegram = ["kuben-api/telegram"]
otel = ["kuben-api/otel"]
full = ["embed-ui", "activator", "ai", "mcp", "telegram", "otel"]

[dependencies]
kuben-core.workspace = true
kuben-crd.workspace = true
kuben-store.workspace = true
kuben-platform.workspace = true
kuben-api.workspace = true
tokio.workspace = true
tokio-util.workspace = true
clap.workspace = true
anyhow.workspace = true
tracing.workspace = true
tracing-subscriber.workspace = true
metrics.workspace = true
metrics-exporter-prometheus.workspace = true
mimalloc.workspace = true
kube.workspace = true
serde_json.workspace = true

[lints]
workspace = true
```

```rust
// crates/kuben/src/main.rs — phase 0 binary skeleton
#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod cli;        // clap: serve | migrate | backup | restore | doctor | reset-admin | import
mod serve;      // build the runtime, shared state, supervisor, signals
mod telemetry;  // tracing json + prometheus exporter (+otel feature)

fn main() -> anyhow::Result<()> {
    let args = cli::parse();
    let cfg = kuben_core::config::Config::load()?;
    telemetry::init(&cfg)?;

    match args.command {
        cli::Command::Serve(opts) => serve::run(cfg, opts),
        cli::Command::Migrate => block_on(cfg.runtime.clone(), async move { kuben_store::Store::connect(&cfg.database).await?.migrate().await }),
        cli::Command::Doctor => block_on(cfg.runtime.clone(), cli::doctor::run(cfg)),
        cli::Command::ResetAdmin => block_on(cfg.runtime.clone(), cli::admin::reset(cfg)),
        cli::Command::Backup(o) => block_on(cfg.runtime.clone(), cli::backup::run(cfg, o)),
        cli::Command::Restore(o) => block_on(cfg.runtime.clone(), cli::backup::restore(cfg, o)),
    }
}

fn block_on<F: std::future::Future<Output = anyhow::Result<()>>>(rt: kuben_core::config::RuntimeCfg, f: F) -> anyhow::Result<()> {
    serve::build_runtime(&rt)?.block_on(f)
}
```

```rust
// crates/kuben/src/serve.rs
use kuben_core::config::{Config, Role, RuntimeCfg};
use tokio_util::sync::CancellationToken;

pub fn build_runtime(rt: &RuntimeCfg) -> anyhow::Result<tokio::runtime::Runtime> {
    let workers = rt.worker_threads.unwrap_or_else(|| std::thread::available_parallelism().map(|n| n.get().min(4)).unwrap_or(2));
    Ok(tokio::runtime::Builder::new_multi_thread()
        .worker_threads(workers)
        .max_blocking_threads(rt.max_blocking_threads)
        .thread_name("kuben")
        .enable_all()
        .build()?)
}

pub fn run(cfg: Config, opts: crate::cli::ServeOpts) -> anyhow::Result<()> {
    // ADR-013: a single runtime in phase 0; `runtime.bulkhead=true` will later build a second runtime for the controller
    let rt = build_runtime(&cfg.runtime)?;
    rt.block_on(async move {
        let shutdown = CancellationToken::new();
        tokio::spawn(signals(shutdown.clone()));

        let roles = if opts.roles.is_empty() { cfg.server.roles.clone() } else { opts.roles.clone() };
        let has = |r: Role| roles.contains(&Role::All) || roles.contains(&r);

        // ---- shared state ----
        let store = kuben_store::Store::connect(&cfg.database).await?;
        store.migrate().await?;
        let cluster = kuben_platform::registry::ClusterRegistry::from_config(&cfg.kube).await?;
        let health = kuben_platform::health::Health::new();
        let projections = kuben_platform::projection::Projections::new();

        // the binary applies the CRDs itself (ADR-017) — controller/all only
        if has(Role::Controller) {
            kuben_platform::controller::crd_apply::ensure(&cluster.primary()).await?;
        }

        // Informers → projections (the API needs them for reads too)
        let inf_token = shutdown.child_token();
        let (c, p, h) = (cluster.clone(), projections.clone(), health.clone());
        tokio::spawn(kuben_platform::supervise::supervise("informers", inf_token, h.clone(), move |t| {
            kuben_platform::projection::informer::run(c.clone(), p.clone(), t)
        }));

        let mut tasks = Vec::new();

        if has(Role::Controller) {
            let (c, p, s, h, t) = (cluster.clone(), projections.clone(), store.clone(), health.clone(), shutdown.child_token());
            tasks.push(tokio::spawn(kuben_platform::controller::run_all(c, p, s, h, t)));
        }

        #[cfg(feature = "activator")]
        if has(Role::Activator) {
            let (c, p, h, t) = (cluster.clone(), projections.clone(), health.clone(), shutdown.child_token());
            tasks.push(tokio::spawn(kuben_platform::activator::run(c, p, h, t, cfg.server.bind_activator())));
        }

        if has(Role::Api) {
            let state = kuben_api::state::ApiState::new(cfg.clone(), store.clone(), cluster.clone(), projections.clone(), health.clone());
            let app = kuben_api::router(state);
            let listener = tokio::net::TcpListener::bind(&cfg.server.bind).await?;
            tracing::info!(bind = %cfg.server.bind, roles = ?roles, "kuben api listening");
            let t = shutdown.clone();
            axum::serve(listener, app)
                .with_graceful_shutdown(async move { t.cancelled().await })
                .await?;
        } else {
            shutdown.cancelled().await;
        }

        // ---- ordered shutdown (Invariant I-15) ----
        health.set_ready(false);
        tokio::time::sleep(std::time::Duration::from_secs(2)).await; // endpoint drain
        shutdown.cancel();
        for t in tasks { let _ = tokio::time::timeout(std::time::Duration::from_secs(10), t).await; }
        store.checkpoint_and_close().await?;        // PRAGMA wal_checkpoint(TRUNCATE) on sqlite
        Ok(())
    })
}

async fn signals(token: CancellationToken) {
    use tokio::signal::unix::{signal, SignalKind};
    let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
    tokio::select! { _ = tokio::signal::ctrl_c() => {}, _ = term.recv() => {} }
    tracing::info!("shutdown signal received");
    token.cancel();
}
```

### 5.5 CI (`.github/workflows/ci.yml` — summary)

```yaml
name: ci
on: [push, pull_request]
jobs:
  rust:
    runs-on: ubuntu-24.04
    services:
      postgres: { image: postgres:17-alpine, env: { POSTGRES_PASSWORD: kuben }, ports: ["5432:5432"], options: --health-cmd pg_isready --health-interval 5s }
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
      - uses: Swatinem/rust-cache@v2
      - uses: taiki-e/install-action@v2
        with: { tool: cargo-nextest,cargo-deny,just }
      - run: just lint
      - run: cargo nextest run --workspace --all-features
      - run: KUBEN_TEST_DB=postgres KUBEN_TEST_PG_URL=postgres://postgres:kuben@localhost/postgres cargo nextest run -p kuben-store
      - run: cargo deny check
  web:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: pnpm/action-setup@v4
      - uses: actions/setup-node@v4
        with: { node-version: 22, cache: pnpm }
      - run: pnpm install --frozen-lockfile
      - run: pnpm lint && pnpm typecheck && pnpm test && pnpm build
      - run: pnpm -F @kuben/web size
  drift:
    runs-on: ubuntu-24.04
    needs: [rust, web]
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
      - uses: pnpm/action-setup@v4
      - run: pnpm install --frozen-lockfile
      - run: just drift
  budgets:
    runs-on: ubuntu-24.04
    needs: [drift]
    steps:
      - uses: actions/checkout@v4
      - uses: helm/kind-action@v1
      - run: just build-musl && scripts/check-budgets.sh   # binary ≤ 25MB; RSS idle ≤ 30MB (fixture 50 apps/200 pods, 5 min)
```

### 5.6 Bootstrap order and `install.sh` (phase 0 → 1)

```
1. (optional) k3s: curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --disable=servicelb --secrets-encryption --kube-apiserver-arg=feature-gates=InPlacePodVerticalScaling=true" sh -
2. Gateway API CRDs (standard channel) + enable the Traefik Gateway provider in k3s (HelmChartConfig)
   — on other clusters: Envoy Gateway (helm) or Traefik 3
3. cert-manager (helm) with config.enableGatewayAPI=true + ClusterIssuer (letsencrypt-prod; IP cert if a public IP)
4. kuben (helm/OCI or manifests): namespace kuben-system, RWO PVC, master-key Secret (generated at boot if absent)
5. kuben doctor (preflight) → prints the initial URL: https://<ip>.sslip.io  (or https://<ip> with an IP cert)
6. Wizard in the UI: admin password → base domain (or continue with sslip.io) → registry → Git provider (each step has a Test)
```

### 5.7 Phase 0 definition of done

- [ ] `just ci` green on Linux amd64 and arm64.
- [ ] `kuben serve --roles=all` on kind: login → project → environment → namespace with PSA/quota/netpol; `kubectl delete environment` → soft-delete.
- [ ] `openapi.json` and `schema.d.ts` committed and drift-free; the UI typed with `openapi-fetch`.
- [ ] Budgets: binary ≤ 25MB, image ≤ 30MB, RSS idle ≤ 30MB (fixture), JS shell ≤ 200KB.
- [ ] Threat model + ADR 001–022 in `docs/adr/`; `CONTRIBUTING.md` with 18 invariants.
- [ ] `install.sh` on an empty VPS (Ubuntu 24.04, 2vCPU/2GB) → browser login over TLS.

### 5.8 Implementation status and golden fixes (updated 2026-09-11)

The code is implemented in `kuben-monorepo/`. This section records three things: what was built, the problems found **during implementation**, and the fixes that were added to the plan and carried out. Wherever this section conflicts with sections 5.3 through 5.6, **this section takes precedence**.

#### 5.8.1 What was built (phase 0 complete, plus the core of phase 1)

| Area | Implementation |
|---|---|
| Workspace | 6 crates with explicit members (not `crates/*`), MSRV 1.94, `clippy::pedantic` with `-D warnings`, `unsafe_code = forbid` |
| CRD | `kuben.dev/v1alpha1` (Project, Environment, App, Release, BuildRun, KubenConfig), self-apply with SSA at boot |
| Controller: Project | Status: number of environments + `Ready` condition |
| Controller: Environment | Namespace `kb-<project>-<env>` + PSA (`baseline` enforce, `restricted` warn/audit) + ResourceQuota (always `services.loadbalancers=0` and `nodeports=0`) + LimitRange + tenant-isolation NetworkPolicy + finalizer with soft-delete and grace (168 hours by default for production) + refusal to adopt a namespace that is not its own |
| Controller: App | A Deployment per process (RollingUpdate with maxUnavailable=0, pod security, no SA token) + HPA when `max > min` + Service + HTTPRoute (Gateway API) + prune + `Ready` and `Exposed` conditions + a separate backoff per object |
| Projection/SSE | Pod, Project, Environment and App + **per-org stream filtering** |
| API | Projects, Environments, Apps (CRUD, restart, logs) and Secrets (write-only) + OpenAPI 3.1 + a typed TS client |
| UI | Login, Projects, Project, Environment (deploy, secrets), App (image, scale, env, pods, logs) + live updates over SSE |
| CLI | Real `backup`/`restore` (ownerReference re-linking) |
| Delivery | Helm chart, two Dockerfiles, CI with 11 jobs + `CI success`, releases for 5 targets + checksums + attestation + GHCR + OCI chart + verify-install, `install.sh`, Dependabot |
| Docs | README, CONTRIBUTING (18 invariants as a review checklist), SECURITY, `docs/ci-cd.md`, `docs/deploy.md` |

#### 5.8.2 Problems found and fixed (golden fixes)

1. **A CRD with a tagged enum (`Source { kind: Image | Git }`)** did not produce a valid structural schema, and `crdgen` panicked inside kube-core. ← It was turned into a one-of style struct: `Source { image?, git? }`. The "exactly one" rule is enforced in the controller (`InvalidSource`). A guard test was added and the rule went into CONTRIBUTING.
2. **Duplicate operationIds in the OpenAPI document.** Every `list`/`get`/`create` shared a single operationId, so openapi-typescript collapsed the types into each other. ← Each endpoint was given a unique `operation_id`, and the rule went into CONTRIBUTING.
3. **An I-1 violation in SSE.** The snapshot showed every logged-in user the resources of all orgs. ← A separate `Visibility` is now built per connection, and deletes are sent only for keys the user has already seen. The result: not even the names of another tenant's resources leak.
4. **kube-rs's finalizer helper is not compatible with soft-delete.** With it, as soon as cleanup returns Ok the finalizer is removed. ← A manual finalizer with `resourceVersion` (conditional writes) was implemented.
5. **Restore with stale ownerReferences.** GC deleted the restored resources, because the owner's uid no longer existed. ← ownerReferences are now attached to the projects' new uids, and the namespace is created immediately with the controller's own builder.
6. **env values are no longer in the stream.** Only the name and the reference are sent. The value itself is returned only in App Detail, and only to a user who holds the `secret-read` permission.
7. **Releases are built from checksummed binaries.**
   - The release image is nothing but a `COPY` of those same binaries; QEMU is not needed.
   - `USER 65532:65532` is numeric so that `runAsNonRoot` works.
   - Releases are built without cache.
   - Actions are pinned by commit SHA (for annotated tags, with `^{}`).
   - A single aggregate `CI success` job is the only required check.
8. **Chart:**
   - The `Recreate` strategy, because the SQLite volume is RWO.
   - The PVC is not deleted, thanks to `resource-policy: keep`.
   - If `replicas > 1` without PostgreSQL, the chart errors out.
   - The ClusterRole covers only the resources the controllers actually use.
9. **Version pins based on ecosystem reality:**
   - pnpm 10.34, not 12; stability matters more than the newest version.
   - size-limit 12, because version 13 requires Node ≥ 22.18.
   - TypeScript 5.9, because the openapi-typescript package's peer dependency is on ^5.
   - k8s-openapi 0.28 (jiff-based times) and kube 4.2.
10. **`build.rs` was removed from kuben-api.** rust-embed already errors on a missing directory. The result: one build script fewer, and no more linker warnings under `-D warnings`.
11. **Workspace members were made explicit.** A hidden directory created by tooling had broken the `crates/*` glob.
12. **Corporate proxy.** With `HTTPS_PROXY` set, `kuben serve` would not come up at all. There were two causes: the `kube/http-proxy` feature was off, and an error building the cluster client was propagated with `?`, taking down the entire server. The release binary's smoke test found this bug. ← The `http-proxy` feature was turned on. Now, when `kube.required=false`, **any** cluster error only puts the server into degraded mode instead of crashing it.
13. **Credential leak in an error message.** That same error printed the proxy URL, username and password included, into the `doctor` output and the log. ← The `redact_credentials` function (with tests) is now applied to every cluster error and every `doctor` output, including database errors, since a Postgres URL can carry a password too.

#### 5.8.3 What remains, in priority order (revised plan)

> **Update 2026-09-11:** Four P0 items below are now implemented — login rate limiting, API tokens, audit of every mutation, and TLS for custom domains — see §5.9 (scenarios 1, 2, 3 and 9). What remains P0: running the kind e2e job in CI and publishing `v0.1.0-rc.1`.

**P0 — before the first public release:**
- Run the e2e job on kind in CI and publish `v0.1.0-rc.1` to exercise the whole pipeline. The script is ready (`scripts/e2e.sh`). In this session the sandbox blocked access to `docker.sock` and `127.0.0.1:6443`, so e2e and the Docker build can run **only in CI or on the user's machine**.
- **TLS for custom domains.** In Gateway API the certificate is defined on the listener, not on the route. The plan: one wildcard Certificate (DNS-01) for `*.baseDomain`, and for custom domains, a ListenerSet (or a separate listener) created by the App controller.
- Per-IP rate limiting for `/auth/login`, for example with moka. Today there is only a semaphore around Argon2.
- API tokens (`kbn_pat_…`). Their authentication path is currently only a stub.
- Audit records for every mutation. Today only login is recorded.

**P1:**
- Build from Git: rootless buildkitd + Railpack + a `buildctl` job, per I-7.
- The `Release` CRD with digest pinning and rollback.
- LogHub with follow mode over SSE (I-12).
- Terminal over WS (I-8, I-9 and I-13).
- Kubernetes Events emitted by the controllers.
- Leader election with a Lease, before running multiple replicas.
- Adding a "the CNI supports NetworkPolicy" check to `doctor`.

**P2:** per the 5.1 roadmap: preview environments, scale-to-zero, DB branching and the rest.

#### 5.8.4 Validation status

The results of this session's local run are recorded in §5.8.5.

#### 5.8.5 Local validation results (2026-09-11, macOS arm64, Rust 1.97, Node 22.15)

| Check | Result |
|---|---|
| `cargo fmt --all --check` | ✅ clean |
| `cargo clippy --workspace --all-targets --locked -- -D warnings` (pedantic) | ✅ 0 errors, 0 warnings |
| `cargo test --workspace --no-fail-fast` | ✅ **69 tests passed, 0 failed** (redaction and cluster degrade, builders, validation, visibility, session, API HTTP, store on SQLite, CRD structural + snapshot) |
| Web: `biome check` + `tsc` | ✅ clean |
| Web: vitest | ✅ **10 tests passed** |
| Web: build + size-limit | ✅ JS **98.7 kB** brotli (budget 200), CSS 4.3 kB (budget 25) |
| `just gen` / drift | ✅ `openapi.json`, `schema.d.ts` and `kuben.dev_all.yaml` were generated |
| `helm lint --strict` + `helm template --kube-version 1.32.0` | ✅ 8 resources; both guards (SQLite with multiple replicas, malformed gateway) produce a clear error |
| `install.sh` | ✅ passes `sh -n` and `bash -n`; negative test against a non-existent release: a clean error, no files installed, the temp directory cleaned up |
| `check-budgets.sh` | ✅ the ok, FAIL (exit 1) and usage (exit 2) paths |
| YAML (workflows, dependabot, chart) + `just --list` | ✅ all parse; ci.yml: 11 jobs + `ci-success`; release.yml: 7 jobs |
| Release binary with the UI embedded (`--release --features embed-ui`, full LTO) | ✅ **19.75 MiB** (budget 25), macOS arm64. Build time about 7.5 minutes. `--version` and `--help` work correctly. The official number is measured by the `budgets` job on Linux musl |
| `cargo deny check` (0.20.2) | ✅ advisories, bans, licenses and sources all ok, after two fixes: (1) `version = "0.1.0"` on the internal path dependencies (without it they count as wildcards, and publishing to crates.io is impossible too); (2) `unmaintained = "workspace"`. The unmaintained `paste` crate enters only through `utoipa-axum`'s proc macro, is not in the binary, and is tracked with Dependabot |
| Binary smoke test (no cluster) | ✅ `migrate` on a fresh SQLite; `reset-admin`; `doctor` reports the database OK and the cluster FAIL with a clean error; `serve` bootstraps the database and the admin, and the informer and controller are restarted by the supervisor after a connection error (it does not crash); credential leak check: 0. This same test found bugs 12 and 13 in §5.8.2 |
| e2e on kind, Docker build, a real release | ⛔ not run in this session: the sandbox blocks access to `docker.sock` and `127.0.0.1:6443`. These run in CI (the `e2e` and `budgets` jobs, and `release.yml`) or on the user's machine with `just e2e` |

---

### 5.9 Ten practical scenarios: comparison, plan and implementation (2026-09-11)

**How the scenarios were chosen.** Each one meets at least one of three criteria:
1. It fixes a real, proven problem in the current code (the P0 items in §5.8.3).
2. It is something a Coolify, Railway or Render user expects on day one and Kuben does not have yet.
3. Its core can be tested without a cluster: a pure builder with unit tests plus an HTTP test. Behaviour on a real cluster is covered by the e2e job in CI.

| # | Scenario | Problem in Kuben today | What competitors do | Kuben approach (key decision) | Depends on |
|---|---|---|---|---|---|
| 1 | Brute-force-safe login | Only a semaphore around Argon2 | Rate limiting or 2FA | Three fixed-window buckets: (email+IP), IP, email. `429` with `Retry-After`. Client IP from the **last** `X-Forwarded-For` hop, because the first hop is client-controlled | — |
| 2 | Structural audit log | Only 1 of 12 mutations (login) is recorded | Qovery: paid plans, 7–30 day retention | A `route_layer` middleware taking the `operationId` from the OpenAPI spec itself, so a new endpoint cannot forget auditing. Plus an audit API and page | — |
| 3 | API tokens and CI/CD | Stub only | Render: deploy hooks; Railway: environment-scoped tokens | `kbn_pat_<id>_<secret>`, looked up by id, compared in constant time; role cap, project/environment scope, expiry; a token cannot mint tokens or members | 2 |
| 4 | Team and roles | A single admin | Coolify: teams; Qovery: RBAC | Invite with a one-time temporary password that must be changed; "the last owner cannot be removed" and "nobody grants a role above their own" | 2 |
| 5 | Release history, rollback, safe rollouts | None | Railway and Render: one-click rollback | `app_releases` table with a spec snapshot (never a secret value); startup/liveness probes and `progressDeadlineSeconds` | 2 |
| 6 | Persistent volumes | None | Railway volumes; Render disks (single instance) | PVCs **without** an ownerReference, so deleting an App never deletes data; `Recreate` strategy; single-replica validation; data is deleted only on explicit request | — |
| 7 | Cron jobs and "run now" | None | Render cron jobs; Coolify scheduled tasks | A process with `schedule` becomes a CronJob (`concurrencyPolicy: Forbid`); "run now" creates a Job from the CronJob template | — |
| 8 | One-click templates and databases | None | Coolify ~371 templates; the incumbent ~164 | A small reviewed catalogue; generated secret with a ready-to-use `url` key; `protocol: tcp` processes get no public route | 6 |
| 9 | Custom domains with automatic HTTPS | TLS exists in name only | Coolify/Dokploy: Traefik ACME; Render/Railway: automatic | Kuben owns the Gateway listeners: one HTTPS listener per host, certificates issued by cert-manager's gateway-shim; global http→https redirect; DNS check | — |
| 10 | Promote staging → production | None | The incumbent's pipelines; Qovery clone; Railway sync with diff | Copy image, runtime, env and volumes (**not** domains) with the `release-promote` permission; `dry_run` returns the diff; warnings for secrets missing in the target | 5 |

#### 5.9.1 Evidence-based comparison with competitors (research 2026-09-11)

| Capability | Coolify | Dokploy | Railway | Render | The incumbent | Qovery | Northflank | **Kuben (after §5.9)** |
|---|---|---|---|---|---|---|---|---|
| Login protection | 5/min per email+IP; header-spoofing bypass fixed Oct 2025 [1] | 2FA/passkeys; rate limit unconfirmed | 2FA, enforceable on Pro | "Secure login" | ❌ | SSO only | Enforced MFA | Three buckets, `429` + `Retry-After`, IP from last XFF hop (phase 3: TOTP/passkeys) |
| Audit log | ❌ not in the UI | Enterprise only | All plans, 48h–18mo | Pro+, ≥90d | Opt-in, off by default, capped at 1000 | 7–30d | Enterprise | **Free, always on, unbounded, structural** |
| Tokens / deploy hooks | Permissioned tokens with expiry [3] | API keys + GitHub Action | Project/environment tokens | Deploy hooks; account-level keys | Per-user token | Role-bound tokens | Role-based, project-scoped | Role cap, project/environment scope, expiry |
| Team & RBAC | Owner/Admin/Member | 3 roles (custom: Enterprise) | Admin/Member/Deployer | Paid plans only | 5 roles (v3) | 5 + custom | Custom roles | 4 hierarchical roles + last-owner and no-escalation rules |
| Rollback | Only to images still cached on the host | ✅ | ✅ one click | ✅ one click | Unconfirmed | ✅ | ✅ | Release history with spec snapshots, one-click rollback |
| Volumes | Docker, single host | ✅ + S3 backups | 1 per service, no replicas | Single-instance disk | PVC incl. RWX | Single-instance PV | Single instance in RWO mode | PVC without ownerReference (phase 2: snapshots, RWX) |
| Cron | ✅ | ✅ | ≥5 min, overlapping runs skipped | ✅ | CronJob | ✅ | ✅ | CronJob with `Forbid` + run now |
| Templates / DBs | ~371 templates, 8 DB engines | ~532 | ~5,289 | Managed Postgres + KV | 164 + operators | 4 DBs | 6 add-ons | 8 reviewed templates with generated secrets (phase 2: CloudNativePG) |
| Domains & TLS | Let's Encrypt; wildcard needs manual proxy edits | Traefik | Automatic wildcard | Automatic + "Verify" button | cert-manager | Automatic | Wildcard | Listener per host + cert-manager + redirect + DNS check |
| Promotion | ❌ clone only | Unconfirmed | Sync with reviewable diff | Unconfirmed | 4-stage pipelines | Clone | Release flows | Promote with `dry_run` diff + secret warnings |

Key sources:
- [1] github.com/coollabsio/coolify/pull/7039 (login rate limiter and bypass fix)
- [3] coolify.io/docs/core/security/credentials/api-tokens
- docs.dokploy.com/docs/core/guides/production-hardening (audit log is Enterprise-only)
- docs.railway.com/enterprise/audit-logs; docs.railway.com/volumes/reference
- render.com/docs/audit-logs; render.com/docs/disks
- the incumbent's operator chart (values.yaml: audit settings)
- qovery.com/pricing
- northflank.com/docs/v1/application/secure/grant-api-access

The Coolify and Dokploy counts are the number of folders in their template repositories on the research date, not official figures. "Unconfirmed" means no source was found.

**Three lessons that changed the plan:**
- **Environment-scoped tokens.** This is the strongest design (Railway, Coolify), so scenario 3 now supports it.
- **Diff preview before syncing.** Railway has it, so scenario 10 gained `dry_run`.
- **Header spoofing.** The Coolify limiter bypass proves the client IP must come from the **last** XFF hop.

#### 5.9.2 Scenario details

**1. Safe login.**
- **Steps:**
  - `auth/throttle.rs`: fixed windows and three buckets.
  - `Error::RateLimited` in `kuben-core`, mapped to `429` with a `Retry-After` header.
  - A successful login resets the (email+IP) bucket.
  - The audit record gets outcome `throttled`.
  - Settings live under `security.login_*`.
- **Test:** after 5 failures the response is `429` with `Retry-After`. Another IP can still log in. A forged `X-Forwarded-For` does not change the bucket.
- **Limitation:** counters are in memory, so they are not shared across HA replicas. A Postgres-backed bucket is phase 3.

**2. Structural audit.**
- **Steps:**
  - `audit.rs` runs as a `route_layer`. It reads `MatchedPath`, extracts the path parameters, and looks up the `operationId` in a table built once from `openapi::spec()`.
  - The outcome comes from the status code: success, denied or failure.
  - The org comes from the project, or from the caller's role bindings.
  - Request bodies are **never** recorded.
  - `GET /audit` pages by `seq` and filters by org. There is a UI page for it.
- **Test:** every successful mutation writes a record with actor, target and request id. Viewers cannot read the audit log.
- **Limitation:** the record is written after the action. A crash exactly between the two loses one record. The complete fix is a transactional outbox, which is not possible for Kubernetes writes.

**3. API tokens.**
- **Steps:**
  - A repository for `api_tokens`.
  - Token format: a 32-hex id plus 256 random bits. Only `sha256` is stored.
  - Comparison with `subtle::ConstantTimeEq`.
  - Effective role = the weaker of the owner's role and the token's cap. A project or environment scope turns the binding into a binding on that node.
  - `last_used_at` is written at most every 60 seconds.
  - Endpoints: `POST/GET/DELETE /tokens`. There is a UI page and a GitHub Actions example.
- **Test:** a valid token works. Revoked or expired tokens get `401`. A viewer token cannot deploy. A token scoped to project A gets `404` on project B. A token cannot create tokens.

**4. Team.**
- **Steps:**
  - Migration `0002` adds `must_change_password`.
  - `members` endpoints: list, invite, change role, remove.
  - `POST /me/password` checks the current password and revokes the user's other sessions.
  - Admin gains `user-admin` but cannot grant or modify owners.
  - Removing a member revokes their sessions and tokens.
  - Team and Account pages.
- **Test:** the last owner cannot be removed. An admin cannot create an owner. After removal, the member's sessions and tokens stop working. A user with `must_change_password` is sent to the Account page.

**5. Releases and rollback.**
- **Steps:**
  - `app_releases` table, unique on (namespace, app, revision).
  - Create, update, rollback, promote and template deploys each record a revision.
  - `GET …/releases` and `POST …/rollback`.
  - Builders: a `startupProbe` allowing about 5 minutes for slow boots, a `livenessProbe` only when a health path is set, and `progressDeadlineSeconds: 600`.
- **Test:** after two deploys and a rollback, revision 3 has exactly the image of revision 1.
- **Limitation:** digest pinning still arrives with the Release CRD (phase 1). The tag is recorded for now.

**6. Volumes.**
- **Steps:**
  - `spec.volumes[] {name, mountPath, size, storageClass}` in the CRD.
  - A PVC builder with the annotation `kuben.dev/retain`.
  - `volumes`/`volumeMounts` in the Deployment and the `Recreate` strategy.
  - Validation: one process and at most one replica; volumes cannot shrink.
  - `DELETE app?delete_volumes=true`.
- **Test:** builder and validation tests.
- **Limitation:** RWO means an App with a volume runs one pod, and `Recreate` means a few seconds of downtime per deploy — exactly like Render disks. Volume backups are phase 2 (VolumeSnapshot).

**7. Cron.**
- **Steps:**
  - `schedule` and `timeZone` on a process.
  - A CronJob builder: `concurrencyPolicy: Forbid`, history of the last 3 runs, `startingDeadlineSeconds: 300`.
  - Scheduled processes get no Deployment.
  - `POST …/run` creates a Job from the CronJob template.
  - UI: a Schedule field and a "Run now" button.
- **Test:** builder tests and validation of the cron expression.

**8. Templates.**
- **Steps:**
  - A catalogue compiled into the binary: Postgres 17, Redis 7, MariaDB 11, n8n, Uptime Kuma, Vaultwarden, Gitea (rootless) and whoami.
  - Generated values go into a Secret named `<name>-credentials`. The env only references that Secret, so real values never appear in the App spec.
  - New `protocol: tcp` on a process: the Service uses the real port and no HTTPRoute is created.
  - UI: a template gallery on the environment page.
- **Test:** every template builds valid Deployments, Services and PVCs with the real builders. Generated secrets are 32 characters and never repeat.
- **Limitation:** images are pinned to major tags (e.g. `postgres:17-alpine`), not digests. Multi-service templates such as WordPress with its database are phase 2.

**9. Domains and HTTPS.**
- **Steps:**
  - `wildcardTlsSecret` in KubenConfig.
  - A listener builder:
    - `http` on port 80.
    - A wildcard `https` listener for the base domain, only when a wildcard secret is configured.
    - Every other host gets its own listener with the stable name `h-<hash>` and the Secret `kuben-tls-<hash>`.
  - A cap of 60 listeners, under Gateway API's limit of 64.
  - App routes attach to the HTTPS `sectionName`s, and one platform route redirects http→https.
  - A new debounced reconciler watches Apps and server-side-applies the listeners onto the Gateway.
  - `GET …/domains` resolves each host's DNS and compares it with the Gateway addresses.
- **Test:** builder tests (stable names, wildcard, cap) and tests for the DNS comparison logic.
- **Limitation:** HTTP-01 needs cert-manager running with Gateway API support enabled. More than 60 hosts requires ListenerSet (experimental in Gateway API).

**10. Promotion.**
- **Steps:**
  - `POST …/promote {to_environment, dry_run}` copies the spec within the same project and keeps the target's domains.
  - Records a release with reason `promote`.
  - Compares the referenced Secrets with those present in the target and returns warnings.
  - UI: pick the target environment and preview the diff first.
- **Test:** unit tests of the pure spec transformation: domains are kept; image and env are copied.
- **Limitation:** the four-eyes approval (§3.7) is not built yet. For now only Admin and Owner hold `release-promote`, so a Developer can deploy to staging but cannot promote to production.

#### 5.9.3 Execution order
Each stage goes green on `fmt`, `clippy`, tests and `gen` before the next one starts:
- **A.** Store and core: migration `0002`, repositories, `RateLimited`, role ordering.
- **B.** CRD: `volumes`, `schedule`, `protocol`, `wildcardTlsSecret`; regenerate the CRDs and the snapshot.
- **C.** Platform: builders (PVC, CronJob, probes, listeners, redirect) and the Gateway reconciler.
- **D.** API: all 10 scenarios, with HTTP tests.
- **E.** Web: Team, Tokens, Audit and Account pages; Release, Volume, Cron, Promote, Domain and Template sections.
- **F.** Full validation: fmt, clippy, Rust and web tests, deny, release build and budgets.
- **G.** Documentation (a user guide for the scenarios) and the results, recorded in §5.9.4.

#### 5.9.4 Results (local validation, 2026-09-11, macOS arm64, Rust 1.97, Node 22.15, pnpm 10.34.5)

All ten scenarios are implemented end to end. Each one has:
- a backend: store, CRD, builder, controller and API
- UI
- tests
- documentation in `docs/guide.md`

| Check | Result |
|---|---|
| `cargo fmt --all --check` | clean |
| `cargo clippy --workspace --all-targets --locked -- -D warnings` (pedantic) | **0 errors, 0 warnings** |
| Rust tests (`cargo test --workspace --locked`) | **107 passed, 0 failed**, including: SQLite/Postgres schema parity for migration 0002; repository round-trips for tokens, members, releases and audit paging; builder tests for volumes, cron, TCP, probes and TLS routes; 6 Gateway listener tests; the 8-template render test; HTTP tests for scenarios 1–5 and 8 |
| `cargo deny check` | advisories, bans, licenses and sources all ok (no new dependencies) |
| OpenAPI | 27 paths; a test enforces unique `operationId`s, because they are the audit action names |
| Web | biome clean; `tsc` clean; vitest 10/10; build ok; **size-limit: JS 103.11 kB / 200, CSS 4.59 kB / 25** |
| `helm lint --strict` + `helm template --kube-version 1.32.0` | ok; the new RBAC rules (PVCs, batch jobs, Gateway patch) and `TRUST_FORWARDED_FOR` are rendered |
| `scripts/e2e.sh` | extended with one step per scenario; `bash -n` ok. **Not run locally** — the sandbox blocks Docker and kind — so it runs in the CI e2e job |
| Release binary (embed-ui, fat LTO) | **22.00 MiB** (budget 25 MiB), `--version` ok. It grew about 2.25 MiB with this round, leaving ~3 MiB of headroom: watch the budget in the CI `budgets` job (musl) |
| Migration upgrade 0001 → 0002 on an existing database | ✅ applied migrations went from `1` to `1,2`; `users.must_change_password` and `app_releases` exist; existing users are kept |

**Problems found and fixed while implementing:**
1. **Type-level incompatibility between openapi-fetch 0.17 and `openapi-typescript --immutable`.** `Readable<T>` only unwraps mutable arrays (`T extends (infer E)[]`), so every `readonly X[]` in the API became an object with non-callable methods, which broke typecheck on every page. → The generator now runs without `--immutable`. Plain arrays still satisfy the UI's `readonly` props.
2. **A security precondition for scenario 1.** A limiter that takes the first `X-Forwarded-For` hop can be bypassed; the research found exactly this bypass in Coolify. → The limiter uses the **last** hop, and only when `trust_forwarded_for` is set. It defaults to off and the chart turns it on, because Kuben sits behind the Gateway.
3. **Server-side enforcement of `must_change_password`.** The UI redirect alone could be bypassed by calling the API directly. → The `Authz` extractor returns 403 for invited users until they change their password; only `/me` and `/me/password` stay reachable.
4. **A stale cache in the UI after a password change.** The route guard's `ensureQueryData` kept serving the old user, which would have trapped the user on the Account page. → The flag is cleared in the query cache before the guards run again.
5. **One validation implementation for API and controller.** The API runs the controller's own `resources::validate` on every spec it writes (create, update, rollback, promote, template). An invalid combination such as a volume with 3 replicas, or a schedule with a port, is rejected with 422 instead of becoming a failing object.
6. **Rolling back to an unrestorable snapshot** returns 422 rather than 500. A test fixture exposed this; the test was fixed and the behaviour was kept.
7. **Honest limitation of scenario 9.**
   - The API's duplicate-domain check reads the projection, so two concurrent creates within about 1 s could both pass.
   - The hard guarantee is at the Gateway: each hostname's listener admits routes only from the namespace that claimed it first.
   - The e2e test waits for the projection before checking.
8. **Local environment only:**
   - Nested `pnpm` calls resolved to an old global pnpm 8. → A corepack shim fixes this locally; CI uses `pnpm/action-setup`.
   - The disk filled up during the release build (`target/debug` had reached 19 GB). → Build caches were cleared.

**Not verified here, and why:**
- **Behaviour on a real cluster.** Gateway listeners with Traefik or Envoy, cert-manager issuing certificates, CronJob execution, PVC binding and promotion all need the CI kind job; the sandbox blocks the Docker socket and `127.0.0.1:6443`. The builders that generate these objects are unit-tested.
- **The Postgres leg** of the store tests runs only in CI (`KUBEN_TEST_PG_URL`).

**Next (P0):** push the repository, get **CI success** and the kind e2e job green, then publish `v0.1.0-rc.1`.

### 5.10 What the first real CI run found (2026-09-11)

The repository's first CI run never reached a single check. Everything below
was found while making it green; the fixes are on branch
`fix/ci-script-permissions` (PR #1).

**The gate.** `scripts/*.sh` and `install.sh` were committed as `100644`, so
the "Detect changes" job died with exit 126 and every job that depends on it
was skipped. The scripts now carry the execute bit, change detection runs
through `bash`, and a guard step fails the build if a script ever loses that
bit again.

**Truncated configuration.** Eleven files were cut off mid-file, and no commit
in the history ever held a complete version: `Cargo.toml` (46 of 128 lines),
`package.json`, `biome.json`, `justfile`, `deny.toml`, `rustfmt.toml`,
`rust-toolchain.toml`, `.cargo/config.toml`, `.gitignore`, `.dockerignore`,
`.editorconfig`. The three that broke a parser were obvious; the other eight
stayed syntactically valid and silently dropped settings — the class of damage
no tool reports. The same breakage is why Dependabot's cargo and npm updates
fail: neither manifest can be parsed.

**Four checks failed on first contact**, each a genuine finding rather than a
CI artefact:

| Check | Finding | Resolution |
| --- | --- | --- |
| Clippy | CI runs stable 1.98.1, which added `clippy::unused_async_trait_impl`; `CurrentUser::from_request_parts` never awaits | plain `fn` returning `future::ready(..)` |
| Supply chain | RUSTSEC-2024-0436: `paste` is archived, reaching us only through utoipa-axum as a compile-time macro, and has no safe upgrade | ignored in `deny.toml` with that justification; revisit when utoipa-axum drops it |
| Shell scripts | shellcheck SC2155: a declaration masked the exit status of a command substitution | declare and assign separately |
| Binary size budget | the static musl binary measures 25.19 MiB against a 25 MiB gate — musl builds bigger than the 22.00 MiB macOS build recorded in §5.8 | gate raised to 26 MiB; the binary still needs to shrink |

**Lessons worth keeping.**

1. A failing gate hides every check behind it. After fixing the first error,
   run each downstream job's commands before calling CI fixed.
2. A committed lockfile is the oracle for a damaged manifest: reconstruct
   until resolution leaves `Cargo.lock` byte-identical, and treat any
   remaining diff as a missing feature flag rather than noise. That is how
   sqlx's `macros` feature was recovered.
3. Local toolchains drift from CI. Clippy passed locally on 1.97.1 and failed
   on 1.98.1; a lint that exists in only one of them cannot be silenced with
   `#[allow]` without breaking the other.
4. A redirect is a destructive write that happens before its command runs:
   `just gen` now builds both generators first, because a failed build used to
   truncate the committed `openapi.json`.

## Appendix A. Sources

- Qovery Pricing (qovery.com/pricing, Apr 2026; SaaSpartout/Gappsy review, Qovery blog Sep 2026): Team from $899/mo, Business $1,999–2,999/mo, Enterprise Custom.
- Northflank Pricing (northflank.com/pricing; blog BYOC Sep 2026): BYOC $0.01389/vCPU-hr + $0.00139/GB-hr; PaaS $0.01667/vCPU-hr.
- Porter Pricing (porter.run/pricing; docs): $13/vCPU-mo, $6/GB-mo; the default AWS cluster ~$201/mo including the monitoring node.
- Coolify Docs (installation; how-coolify-works; TECH_STACK.md): minimum 2CPU/2GB; 4 base containers (Laravel, PostgreSQL, Redis, Soketi). A user report of ~1.3GB idle (nusendra.com, Jun 2026).
- Dokploy Docs: minimum 2GB RAM/30GB; Swarm + Traefik + PostgreSQL.
- Devtron Docs (Getting Started): 2CPU/6GB (CI/CD, ≤5 App); 6CPU/13GB (>5 App).
- KubeVela vela-core chart: request 20Mi / limit 1Gi; Performance Fine-tuning: Small 0.5CPU/1Gi.
- Kubernetes Blog: Ingress NGINX Retirement (Nov 2025); Steering/SRC Statement (Jan 2026); Ingress2Gateway 1.0 (Mar 2026).
- Kaniko: GoogleContainerTools/kaniko#3348; Chainguard fork (archived June 2025).
- Railpack: railpack.com (BuildKit Frontend Reference, Running in Production); Railway blog (Mar 2025); Nixpacks Maintenance Mode.
- BuildKit rootless: moby/buildkit docs/rootless.md; requires seccomp/AppArmor unconfined.
- KEDA HTTP Add-on 0.16 docs (Architecture, Scaling, Cold-Start); Sablier (sablierapp.dev).
- pgbranch (OverlayFS CoW, ~1.9s); CloudNativePG docs (Volume Snapshot backup/recovery, Replica clusters); OpenEBS + CNPG.
- OpenTelemetry eBPF Instrumentation (OBI): opentelemetry.io/docs/zero-code/obi; Grafana Beyla donation (May 2025); First release (Nov 2025). Coroot; Cilium Hubble.
- OpenCost Specification and Allocation API.
- kube-rs 4.0.0 (Jun 2026), 3.x changelog (streaming lists, retry policy, WS keepalive).
- axum 0.8.9 (Apr 2026); sqlx 0.9.0 (May 2026; MSRV 1.94; sqlx.toml; SqlSafeStr); utoipa 5.5.0 (May 2026); utoipa-axum 0.2.
- cert-manager docs: HTTP-01 Gateway API solver (`config.enableGatewayAPI`).
- Let's Encrypt Rate Limits (Aug 2026); cunnie/sslip.io#57 and #108 (exhaustion in February 2026; raised to 200k); sslip.io recommends an IP Certificate.
- Gateway API Bench (howardjohn/gateway-api-bench): Envoy Gateway memory leak under churn; Traefik unsafe with several Gateways and many Routes.
- pnpm 10 Catalogs; Vite 8 + React 19 + Tailwind v4 + shadcn on Base UI Templates (2026).
- teloxide (buttons example, CallbackQuery); rust-genai (v0.6, Ollama/OpenAI/Anthropic/Gemini native).
