# 🔍 Structural and architectural critique of the Kuben golden document — and the final plan corrections

> A critical review of `KUBEN-GOLDEN-ARCHITECTURE.md` (v1.0) from an outsider's point of view: where it is optimistic, where it carries complexity for no reason, which risks are underestimated, and exactly what changes in the final plan (v1.1).

| | |
|---|---|
| **Version** | 1.0 (critique) → produces plan v1.1 |
| **Date** | 2026-09-10 |
| **Document under review** | [KUBEN-GOLDEN-ARCHITECTURE.md](./KUBEN-GOLDEN-ARCHITECTURE.md) v1.0 |
| **Criteria for the critique** | (1) simplicity versus value, (2) execution risk, (3) feasibility for a team of 2 to 4, (4) security, (5) the real self-host experience |

---

## Contents

- [0. Overall verdict and score](#0-overall-verdict-and-score)
- [1. Structural criticisms of the plan itself](#1-structural-criticisms-of-the-plan-itself)
- [2. Architecture criticisms, point by point](#2-architecture-criticisms-point-by-point)
- [3. Risks v1.0 missed or underestimated](#3-risks-v10-missed-or-underestimated)
- [4. Monorepo structure: criticism and correction](#4-monorepo-structure-criticism-and-correction)
- [5. Final revised plan (v1.1)](#5-final-revised-plan-v11)
- [6. Added ADRs](#6-added-adrs)
- [7. Architecture definition of done, before the first line of code](#7-architecture-definition-of-done-before-the-first-line-of-code)
- [8. Conclusion](#8-conclusion)

---

## 0. Overall verdict and score

**Score for v1.0: 7.5 out of 10.** The directions it picks are right and the analysis of the incumbent PaaS is evidence-based, but the document has the three classic diseases of "architecture on paper":

1. **Gold-plating:** several complex mechanisms (two runtimes, a hash chain in audit, three concurrent transports, 13 crates) whose value has not been shown to justify their implementation and maintenance cost.
2. **Schedule optimism:** phases 0 through 3 add up to roughly 30 weeks to replace a product with 40,000 lines of code, an operator and 160 templates. The real number to reach "parity with the incumbent + Enterprise" with a team of 2 to 4 is **12 to 18 months**.
3. **Three underestimated operational risks:** an in-cluster registry (the chicken-and-egg of node trust in TLS), bootstrapping Kuben itself (gateway, TLS and webhook URL before anything is installed), and the lifecycle of the CRDs themselves (Helm does not upgrade CRDs).

### Change table: v1.0 → v1.1

| # | Topic | v1.0 | v1.1 (correction) | Reason |
|---|---|---|---|---|
| 1 | Two Tokio runtimes | from phase 0 | **One runtime** with strict `spawn_blocking` discipline; two runtimes **behind a config flag** and only after the phase 1 benchmark | Complexity of shared state, a duplicated client, tracing; real starvation is solved with `spawn_blocking` and a semaphore |
| 2 | REST + SSE + WS | three transports | **REST + one SSE per tab (scope = project) + WS for the terminal only**; logs are multiplexed onto that same per-tab SSE | Removes the subscribe API, removes the 6-connection problem, one reconnect path |
| 3 | Audit hash chain | global | **Hash chain removed from the core**; append-only + a periodic signed export (Merkle anchor) in Enterprise | A global chain in a multi-replica Postgres is a serialization point |
| 4 | Session cache | moka with a 60-second TTL | **5-second** TTL + `sessions.revoked_at` + a version counter for replicas | The "instant revoke" claim was false with a 60-second TTL |
| 5 | Build | a Job with an init container for fetch + BuildKit | **A persistent buildkitd (rootless StatefulSet + cache PVC)** + a lightweight `buildctl` Job; fetching done by BuildKit itself (git context); Railpack as a BuildKit frontend; CNB pushed to phase 3 | A persistent cache = builds 10x faster; removes the dedicated fetch image |
| 6 | Internal registry | Zot "optional" | **An external registry is mandatory in phase 1**; internal Zot in phase 2 only with a public domain + a valid certificate (ACME) | The node has to trust the registry; self-signed in Kubernetes is an operational nightmare |
| 7 | Build logs | zstd in SQL | **A file on a PVC or in `object_store`** with retention; only metadata in SQL | SQLite gets large and slow with 5MB blobs |
| 8 | Metrics Lite | depends on metrics-server | metrics-server **if present**; otherwise `kubelet /stats/summary` or showing "Metrics unavailable" with install instructions | metrics-server is not the default on every cluster |
| 9 | Release snapshot | "configuration snapshot" | A snapshot of the secrets' **reference + resourceVersion**, not their value | A secret value never goes into a CR |
| 10 | Crates | 13 crates from day one | **6 crates** in phase 0; split only when compile time or a team boundary demands it | Premature over-splitting = friction for no reason |
| 11 | Roadmap | ~30 weeks to Enterprise | **A 12-week MVP** with a closed scope + 12 to 18 months to phase 3 | Realism |
| 12 | CRD lifecycle | only a versioning policy | **Kuben applies the CRDs itself at boot** (server-side apply) + ratcheting + storage version migration | Helm does not upgrade files under `crds/` |
| 13 | Bootstrap | unwritten | **An explicit bootstrap section**: `sslip.io` as the default domain, port-forward first, install order gateway → cert-manager → Kuben | Chicken-and-egg |
| 14 | Deleting an environment | finalizer | **Soft delete with a 7-day grace period** + a `protected` annotation + typing the name to confirm | Blast radius of deleting a namespace |
| 15 | Kuben's own RBAC | "least privilege" | Honestly: Kuben is a **cluster-admin-lite**; in HA the `api` role gets tighter RBAC than `controller` | Real security, not a slogan |
| 16 | SQLite → Postgres | unwritten | A `kuben db migrate --to postgres` command from phase 2 | The solo → HA growth path |

---

## 1. Structural criticisms of the plan itself

### 1.1 "Everything is day one" means nothing is

In section 8 of the v1.0 document, more than 40 items carry the D1 (day one) label. When everything is the top priority, the team effectively works without priorities and phase 0 turns into a four-month swamp.

**Correction:** three new levels:

- **D1-Structural:** the things that would force a rewrite if added later (the data boundary, the session model, streams being bounded, SSA, idempotency in deploy, projection). These **must** be in phase 0.
- **D1-Stub:** an interface in phase 0 with a simple implementation (leader election as a trait plus a `NoopLeader` implementation; a circuit breaker as a trait; OIDC as just an `IdentityProvider` trait plus a local implementation).
- **D2:** everything else.

The exact list is in section 5.

### 1.2 The timeline is optimistic

| Phase | v1.0 | Realistic estimate (3 Rust-fluent people) | Reason for the gap |
|---|---|---|---|
| 0 | 4 to 6 weeks | 8 to 10 weeks | Full auth + two DBs + CRDs + the OpenAPI pipeline + the UI shell + CI budgets = 6 parallel tracks |
| 1 | 8 weeks | 12 to 16 weeks | A build system always takes 2x the estimate; LogHub and the terminal are 2 weeks each with soak testing |
| 2 | 8 weeks | 16 to 20 weeks | 5 git providers, 3 data services, the importer, the template catalog, the CLI, Helm |
| 3 | 8 to 10 weeks | 12 to 16 weeks | HA and multi-cluster need real chaos testing |
| **Total** | **~30 weeks** | **~50 to 60 weeks** | |

**Correction:** instead of stretching the phases, **cut the scope.** Section 5 defines a "real 12-week MVP" that an actual user can live with.

### 1.3 The document violates its own tenets in places

- Tenet 7 says "zero-ops with an escape hatch", but the plan of internal Zot + cert-manager + gateway + metrics-server + BuildKit drags five external dependencies into the base install. Zero-ops means the installer installs **all of them** with one command and sensible defaults and **shows their health in the UI**. That work was not estimated in any phase.
- Tenet 3 (everything is bounded) is not honored in the release section: "the last N releases in etcd", but N is not defined and no reconciler garbage-collects them.

### 1.4 What the document should have covered but did not

- **A testing model for the controllers** without a cluster (a fake API server with `tower-test` / a `hyper` mock in kube-rs) — without it, every controller test needs kind and CI gets slow.
- **An observability strategy for development itself** (how do we debug a broken reconcile? `kuben debug reconcile app/foo --dry-run` printing the resulting manifests).
- **A versioning and compatibility model** across the CLI, the API, the CRDs and the Helm chart.
- **A formal threat model** (a short STRIDE) — the document saw security as a checklist, not as a threat model.

---

## 2. Architecture criticisms, point by point

For each item: **the v1.0 claim → the problem → the correction.**

### 2.1 Two Tokio runtimes (bulkhead) — premature complexity

**Claim:** two runtimes give CPU isolation and are cheap.

**Problem:**
- "Cheap" is true only in terms of threads. The real cost: every shared type has to be `Send + Sync + 'static` and cross the runtime boundary; `tracing` span context does not propagate between runtimes; two `kube::Client`s, two connection pools, two sets of metrics; tests have to bring both up; a nested `block_on` is a classic panic.
- The starvation the document itself listed (Argon2, large deserialization, YAML) is all solved with `spawn_blocking` + a semaphore, **not** with two runtimes. Two runtimes only protect against "CPU-bound code we forgot to `spawn_blocking`"; and that same code will kill reconciliation on the second runtime too.
- Memory isolation is not achieved at all (the document admits this itself).

**Correction (v1.1):**
- Phase 0: **one runtime** (`worker_threads = min(4, cpus)`, `max_blocking_threads = 16`).
- Lintable discipline: Clippy's `await_holding_lock`, plus a custom lint (or a review rule) requiring that any function named `*_blocking`, or any known CPU-bound crate (argon2, serde_yaml on large input, zstd), is only called inside `spawn_blocking`.
- `tokio::task::Builder` with a name for every task (the `tracing` feature) so `tokio-console` can point at the culprit for starvation.
- **An abstraction for the future:** a `Runtimes { api: Handle, ctrl: Handle }` struct where, in phase 0, both handles point at the same runtime. If the phase 1 benchmark shows tail latency degrading under reconcile load, switching to two runtimes is a 50-line change in `main.rs`.

### 2.2 The data boundary: v1.0 introduced a new split

**Claim:** "CRD = desired state, SQL = identity." Clean.

**Problem:** `Project` and `Environment` are both CRDs *and* referenced from `role_bindings.scope_id` in SQL. When someone deletes the CR with `kubectl delete project foo`, the SQL bindings are orphaned. When SQL is restored but etcd is not (or vice versa), the references become meaningless. **The same split as the incumbent, just with cleaner names.**

**Correction:**
1. **Ownership rule:** every entity has exactly one owner. Org, User, Membership, Session, Token, Audit → SQL. Project, Environment, App, Release, BuildRun, Domain, Service → CRD.
2. **SQL references a CRD only by `uid`** (never by name), plus a **cleanup reconciler** (`BindingGC`) that deletes bindings whose `scope_uid` no longer exists after a 1-hour delay (not immediately, so a partial restore does not destroy data).
3. **Org on the CR via a label:** `kuben.dev/org: <org-id>` on the project. Authorization = (the role binding in SQL) ∩ (the label on the CR). Cached in memory.
4. **Atomic backup:** the `kuben backup` command stores both together with a shared manifest (timestamp + checksum), and `restore` brings both back or neither.

### 2.3 Three concurrent transports (REST + SSE + WS)

**Claim:** SSE for events and logs, WS for the terminal, and "one multiplexed SSE per tab with a subscribe API".

**Problem:** a "subscribe API" means a side POST that the server has to tie to the SSE connection (a session registry, a race between subscribe and the first event, cleanup when the SSE drops). This is exactly what WebSocket gives you natively. The document has WS, and SSE, and a subscribe API; three different reconnect mechanisms in the client.

**Correction (simpler, with no loss of capability):**
- **One SSE per tab with the scope in the URL:** `GET /api/v1/stream?project=<uid>&logs=<app>:<pod>:<container>,...`. A scope change (navigation, or opening the log viewer) = closing and reopening the SSE with a new URL. Because the model is snapshot + delta and we have `Last-Event-ID`, reopening is nearly free. The server only filters by permission.
- **Concurrent log viewers** are multiplexed over that same SSE with event type `log` and a `stream_id` field. LogHub stays unchanged.
- **WS for the terminal only** (binary, backpressure).
- Result: **at most 1 SSE + N terminal WS per tab.** The 6-connection problem effectively goes away, even without HTTP/2.
- **Hard rule:** no other endpoint streams. If someone wants long-polling or a new SSE, they have to write an ADR.

### 2.4 Session cache and the "instant revoke" claim

**Claim:** sessions in the DB, cached in moka with a 60-second TTL, "instant revoke".

**Problem:** with a 60-second TTL, a deleted session stays valid for up to 60 seconds; in HA each replica has its own cache. That is not "instant", and it matters for the "fired employee" or "leaked token" scenario.

**Correction:**
- Cache TTL of **5 seconds** (DB cost: one primary-key point lookup every 5 seconds per active user — negligible).
- Sensitive operations (`app:exec`, `secret:read`, RBAC changes, deletions) **always** read the session from the DB (bypassing the cache).
- In HA: a `revocations (subject_hash, at)` table + every replica reads `MAX(at)` every 2 seconds and invalidates the cache if it changed. Simpler than pub/sub and with no Redis.
- **Lightweight session binding:** store the `ua_hash` and the `/24` of the IP in the session; if both change together → require re-auth. (Not hard binding to the IP; that breaks mobile users.)

### 2.5 Audit hash chain

**Claim:** audit records with `prev_hash`/`hash` for tamper evidence.

**Problem:**
- A global chain = every insert has to read the last hash → writing audit records in a multi-replica Postgres serializes completely (an advisory lock or a retry loop). It creates a bottleneck in exactly the place where HA is needed.
- Tamper evidence is meaningless against someone with DB access, because that same person can rebuild the chain from any point they like. Real value only comes from an **external anchor**.

**Correction:**
- Core: audit is **append-only** (no UPDATE/DELETE, enforced through a restricted DB role) + a monotonic `seq` + export.
- Enterprise: a **periodic Merkle anchor** — every 5 minutes a root hash is computed over the new batch, signed with an Ed25519 key and sent to external object storage or to syslog/SIEM. No serialization in the write path.

### 2.6 Build system — the largest revision

**Claim:** one Job per build with a `fetch` init container + BuildKit; no Kaniko; Railpack and CNB.

**Problems:**
1. **Without a persistent cache, builds stay slow.** Job-per-build with a registry cache helps, but an on-disk layer cache is 5 to 10 times faster. Coolify/Dokploy users are used to 20-second warm builds.
2. A dedicated `fetch` init container (as the incumbent ships one) is yet another image to maintain. **BuildKit itself understands a git context with auth (a secret).**
3. CNB needs a lifecycle image, a builder image and the platform API — an entire subsystem. It has no place in phase 1.
4. Rootless BuildKit on Kubernetes needs a particular `securityContext` (`seccompProfile: Unconfined`, `apparmor: unconfined`, and sometimes `procMount`), which does not fit with PSA `restricted`. The document mentioned this in one line but never operationalized it.

**Correction (v1.1):**
```
kuben-builds namespace  (PSA: baseline, not restricted — explicit and documented)
├── buildkitd            StatefulSet, 1 replica, rootless, PVC cache (default 20Gi, BuildKit's internal GC)
│                        mTLS with a certificate generated by Kuben (not cert-manager; internal)
└── build-<app>-<sha>    Job, image: buildctl (pinned digest), no SA token, NetworkPolicy: buildkitd + registry + git only
                         buildctl build --addr tcp://buildkitd:1234
                           --frontend dockerfile.v0 | --frontend gateway.v0 --opt source=<railpack-frontend@digest>
                           --opt context=git://... (secret: git token)
                           --export-cache type=inline
                           --output type=image,name=<registry>/<app>:<sha>,push=true
                         → termination message: image digest
```
- Controller: it watches Job completion, reads the digest from the `terminationMessage` (or a registry HEAD) and creates the `Release`. The build pod never sees the API server (invariant 5 is preserved).
- **Strategies in phase 1:** `dockerfile` and `railpack` (both BuildKit frontends, one code path). `image` (no build). CNB → phase 3 via `kpack` or `pack` as an integration, not an in-house implementation.
- **Queue:** a `BuildRun` CR + a concurrency limit in the controller (`max_concurrent_builds`, globally and per org). buildkitd manages parallel builds itself.
- **Scale:** for Enterprise, buildkitd goes to N replicas with `buildctl --addr` round-robin, or onto a separate node pool.
- **Fallback without privileges:** if a cluster does not allow rootless BuildKit (some managed clusters with strict policy), the `image` strategy + **remote build** (a GitHub Actions template that pushes the image and fires a webhook). This fallback has to be documented and ready in phase 2.

### 2.7 In-cluster registry — the node-trust chicken-and-egg

**Claim:** internal Zot is "optional for solo installs".

**Problem:** every node's kubelet has to be able to pull from the registry. An in-cluster registry with self-signed TLS = `containerd` on **every node** has to be told to accept that CA (via `/etc/rancher/k3s/registries.yaml` or `/etc/containerd/certs.d/`). That means SSH-ing into the nodes — exactly anti-zero-ops, and impossible on managed clusters. Insecure HTTP has the same problem. In v1.0 this risk got one line; in reality it is **the most common reason Kubernetes PaaS installs fail**.

**Correction:**
- **Phase 1:** an external registry is **mandatory** (ghcr.io, Docker Hub, GitLab Registry, ECR/GCR/ACR, or any OCI registry). The install wizard collects credentials and validates them with a test push. This is what the incumbent effectively expects as well.
- **Phase 2:** internal Zot **only** when (a) the gateway + cert-manager work with ACME and (b) there is a public domain (`registry.<base-domain>`). Nodes trust Let's Encrypt → no touching the nodes. Pulls from inside the cluster go through that same public domain (hairpin); the installer has to check that hairpin NAT works.
- **The k3s path:** on k3s, the installer can enable `--embedded-registry` (Spegel) so pulls are cached between nodes; but that is not a replacement for a registry.
- **UI:** registry status (reachability from the node) is shown as a permanent health check, with a comprehensible error message.

### 2.8 Build logs in SQLite

**Problem:** multi-megabyte blobs inside SQLite: the DB file grows, `VACUUM INTO` for backups gets slow, and the page cache fills with data that matters (sessions, RBAC) being evicted.

**Correction:** each build's log as a `zstd` file on Kuben's own PVC (`/data/build-logs/<app>/<sha>.log.zst`) or in `object_store` (if S3 is configured). Only `(build_id, path, size, sha256)` in SQL. Retention: the last 50 builds per app or 30 days, whichever comes first. Streaming during a build comes straight from the pod log stream (LogHub).

### 2.9 Metrics Lite and metrics-server

**Problem:** metrics-server is present on k3s, but is not the default on EKS/GKE/AKS (or is configured differently). The document promised "no Prometheus" but leaned on another dependency.

**Correction:**
- Auto-detection: if `metrics.k8s.io` exists → poll every 15 seconds (one list for the whole cluster with a label selector).
- If it does not: the installer installs it (a small Helm chart, zero-config); on managed clusters, just a one-line instruction.
- A fallback **with no dependency at all:** `GET /api/v1/nodes/<node>/proxy/stats/summary` (the kubelet summary API via the API server proxy). It needs `nodes/proxy` RBAC and is a bit heavier, but it works everywhere. In phase 2.
- The UI never crashes or shows an empty placeholder; it shows "Metrics unavailable — Install metrics-server" with a button.

### 2.10 Releases and secrets

**Problem:** "release = digest + configuration snapshot", taken literally, puts env values (which include secrets) into a CR that anyone can read with `get releases`.

**Correction:** a release contains: the digest, the app's non-sensitive spec (processes, scaling, domains), and for every secret only `{name, resourceVersion}`. Rollback = restoring the spec + checking that the secrets still exist (if they changed, the UI warns "Secrets changed since this release"). App secrets themselves become immutable-versioned: `app-<name>-env-<hash>` with `immutable: true`, plus GC for unreferenced versions. This gives both an accurate rollback and an automatic rolling update when env changes (because the secret's name changes in the pod template).

### 2.11 Terminal — two forgotten details

1. **The control channel while paused:** in the sample code, `stdout.read` is not run while `paused`, but resize/control messages still arrive from the browser and are handled — correct. However, **the process exiting while paused** is not detected until resume. Fix: add `proc.join()` as a separate `select!` branch.
2. **Ephemeral debug containers:** once created they **cannot be removed** until the pod restarts, and a shared process namespace only works with `shareProcessNamespace` or `targetContainerName`. The UI has to say this ("Debug container stays until pod restart") and audit it. Needs `pods/ephemeralcontainers` RBAC.

### 2.12 mimalloc and the RSS budget

**Problem:** mimalloc returns freed memory to the OS with a delay (purge delay) and reserves 4MiB segments; idle RSS can be 5 to 10MiB higher than with the system allocator — exactly what fights a 25MiB budget.

**Correction:** benchmark all three options in phase 0's CI (musl default; mimalloc with `MIMALLOC_PURGE_DELAY=0`/`mi_option_purge_delay`; jemalloc with `background_thread` and a low `dirty_decay_ms`) and **choose by the numbers**. Initial guess: mimalloc with a short purge.

### 2.13 Frontend — practical details

- **Global 401:** a `QueryClient` interceptor that turns a 401 into a "Session expired" modal and resumes the queries after re-login, rather than a raw redirect. (In the incumbent, once the JWT expires the UI silently goes blank.)
- **Optimistic concurrency:** the app form must send `resourceVersion` with `If-Match`; on conflict → show a diff, do not overwrite.
- **RTL and i18n in phase 0:** logical properties from the start (`ms-`, `pe-`, `start`/`end`), because retrofitting 300 components later is agony; but **actual translation** (Persian, German, …) is phase 2 — only the `paraglide` infrastructure in phase 0.
- **Bundle budget:** xterm + the WebGL addon + uPlot + CodeMirror together come to roughly 150 to 200KB Brotli; the "200KB for the initial shell" budget is only held with lazy routes for the terminal, logs and editor. This has to be enforced in `size-limit` per route, not in aggregate.

---
## 3. Risks v1.0 missed or underestimated

### 3.1 Bootstrapping Kuben itself (chicken-and-egg)

To receive webhooks from GitHub, Kuben needs a public URL; a public URL needs a Gateway + DNS + TLS; and the Gateway has to be installed by Kuben itself. In v1.0 no part of the document defined this ordering.

**Correction — the official bootstrap order:**
1. `install.sh`: (k3s if needed) → Gateway implementation (Traefik or Envoy Gateway) → cert-manager → Kuben (Helm/manifest).
2. Kuben on first boot: applies the CRDs, creates the admin (a one-time password in the log and in a Secret), and is usable **without a domain**: `kubectl port-forward` or NodePort, or the automatic `<ip>.sslip.io` domain with an ACME HTTP-01 certificate (which works with a public IP).
3. Initial wizard: base domain (or continue with sslip.io), registry, Git provider. Every step has a test.
4. As long as there is no public domain, webhooks fall back to **polling** (`ls-remote` every 60 seconds) — without the user noticing. This is the same catch-up mechanism as section 5.5 of v1.0, only permanent.
5. `kuben doctor`: a CLI and a UI page that check all preflights (DNS, hairpin, registry, PSA, RWO StorageClass, metrics-server, Gateway Class).

### 3.2 Lifecycle of the CRDs themselves

- Helm installs the `crds/` directory only on install and never touches it on upgrade. **Kuben must apply its own embedded CRDs at boot with server-side apply** (requires RBAC on `customresourcedefinitions`; in HA mode only the leader). If the RBAC is missing: a clear error log plus a Helm hook Job as the fallback.
- **CRD validation ratcheting** (K8s ≥1.30): adding a new CEL rule must not make existing objects invalid for updates that do not touch that field. Ratcheting is enabled by default but must be tested.
- **Storage version migration:** when `v1beta1` is added, every stored object written as `v1alpha1` must be rewritten (`kube-storage-version-migrator`, or an internal Job that touches them all). State this explicitly in ADR-CRD.
- **Downgrade:** Kuben version N-1 with a CRD of version N must preserve unknown fields (`x-kubernetes-preserve-unknown-fields` on the top-level spec, plus `#[serde(flatten)] extra: BTreeMap`).

### 3.3 Kuben's own RBAC is cluster-admin-lite

Kuben creates namespaces, writes Secrets, RoleBindings and NetworkPolicies in every namespace, execs into pods, and applies CRDs. **Any RCE in Kuben means full takeover of the cluster.** v1.0 wrote "least privilege", which in this position is a slogan.

**The honest correction:**
- In the documentation: "The Kuben admin is effectively cluster-admin. Install Kuben in a dedicated namespace with a strict NetworkPolicy."
- **In HA mode:** the `api` role only has RBAC to read, plus write Kuben CRs, `pods/log` and `pods/exec`; the `controller` role holds the broad RBAC. An RCE in the API can only write CRs (which the controller validates), not Secrets and RoleBindings.
- **In `all` mode:** this separation does not exist — document that explicitly.
- Impersonation (Enterprise): the API works with `Impersonate-User` so that K8s's own audit sees the real user and K8s RBAC becomes a second layer of defense.

### 3.4 Blast radius of deleting an environment

Deleting an environment means deleting the namespace, which means deleting every pod, PVC and Secret. One wrong click, or one bug in a finalizer, destroys production data.

**Correction:**
- `Environment` gets `spec.deletionPolicy: Retain | Delete` (default `Retain` for `production` and `Delete` for `preview`).
- **Soft-delete:** the UI turns a deletion into `status.phase: Terminating` + `deletionScheduledAt = now + 7d`; workloads are scaled to 0 (zero cost), PVCs stay; one-click restore for up to 7 days. After the grace period the reconciler performs the real deletion.
- The annotation `kuben.dev/protected: "true"` on an Environment or App → deletion only by typing the name plus the `env:delete-protected` permission.
- **Never** delete a namespace Kuben did not create (adopted from the incumbent) except behind an explicit flag.

### 3.5 The solo → HA path

A user who started with SQLite and grew must be able to move to Postgres without reinstalling from scratch. v1.0 had no tooling for this.

**Correction:** `kuben db migrate --from sqlite:///data/kuben.db --to postgres://...` (table-by-table copy inside a transaction, verification by row count and checksum, and cutover by changing `DATABASE_URL`). Phase 2.

### 3.6 etcd encryption

Secrets in etcd are not encrypted by default. The k3s installer must enable `--secrets-encryption`; `kuben doctor` should warn on other clusters.

### 3.7 The minimal threat model v1.0 lacked

| Threat | Attacker | Control |
|---|---|---|
| Tenant A sees tenant B's logs/terminal | Authenticated user | Authorization on every subscription (invariant 2); a negative E2E test is mandatory |
| A user's build code attacks the cluster | Malicious developer or poisoned dependency | Isolated namespace, no SA token, restricted egress NetworkPolicy, separate node pool in Enterprise |
| A user's app reaches the API server or Kuben | Compromised container | `automountServiceAccountToken: false`, default-deny NetworkPolicy toward `kube-system` and the Kuben namespace |
| Browser session theft | XSS, malicious extension | HttpOnly cookie, strict CSP, short cache TTL, re-auth for sensitive operations |
| A forged webhook triggers a deploy | Network attacker | HMAC + delivery-ID dedupe + only configured branches |
| Supply chain (a poisoned helper image) | Upstream | All images digest-pinned, Renovate with review, cosign verification for Kuben images |
| RCE in Kuben | Anyone | Section 3.3; Kuben in its own namespace with PSA `restricted` and a read-only root filesystem |
| Malicious admin | Insider | Append-only audit + external export; 4-eyes for production (Enterprise) |

---

## 4. Monorepo structure: criticism and correction

### 4.1 Thirteen crates is too many

Premature over-splitting: every crate boundary means making types `pub`, feature flags that have to be forwarded, and compile units that get rebuilt together anyway. A split pays off when (a) the compile time of one big crate becomes painful, (b) a crate is published independently (`kuben-crd` for third-party tools, `kuben-client` for the CLI), or (c) it is a team boundary.

**The v1.1 structure for phase 0:**
```
crates/
├── kuben-crd/        # CRD types + crdgen  (publishable; no tokio/axum)
├── kuben-core/       # domain, errors, config, ids, policy trait, store trait  (no IO)
├── kuben-store/      # sqlx + sea-query + migrations
├── kuben-platform/   # k8s (registry, informers, projections, loghub, exec) + controllers + build
├── kuben-api/        # axum + openapi + sse/ws + web assets embed + bin: openapi
└── kuben/            # bin: server (roles, supervisor, signals) + cli subcommands
```
- `kuben-auth` moves into `kuben-core` (traits) and `kuben-api` (the HTTP implementation).
- `kuben-controller`, `kuben-k8s` and `kuben-build` go together into `kuben-platform`; split it once it reaches 15 thousand lines.
- `kuben-telemetry` becomes a module inside `kuben`.
- A separate `kuben-cli` **only when** the CLI is published independently (phase 2) — at that point `kuben-client` (generated from OpenAPI with `progenitor`, or hand-written) is added too.
- `kuben-testkit` → shared `dev-dependencies` in `kuben-core` behind a `test-util` feature.

### 4.2 Template catalog

A separate repo from day one (`kuben-templates`), with its own license, CI that validates the schema, and Kuben pulling it as a versioned OCI artifact or tarball (air-gap: bundle it inside the image, or mirror it). The incumbent keeps its templates in the main repo, which ties releases and licensing together.

### 4.3 `apps/docs` with Astro

Not in phase 0. A good `README` plus `docs/` in Markdown is enough until beta. Astro Starlight in phase 2, together with the CLI.

### 4.4 Generated files in git

`packages/api-client/schema.d.ts` and `charts/kuben/crds/*.yaml` are committed (so the frontend can build without a Rust toolchain) and CI catches drift with `git diff --exit-code` — that was right. One addition: a `CODEOWNERS` that blocks changes to these files without a matching change to the source (a review rule).

---

## 5. Final revised plan (v1.1)

### 5.1 A real 12-week MVP ("a developer can live with it")

Closed scope — anything outside this list is a **no** until the MVP ships:

| Area | In the MVP | Outside the MVP |
|---|---|---|
| Auth | Local user + password (Argon2id), cookie session, one org, the `admin`/`developer`/`viewer` roles | OIDC, passkeys, TOTP, multi-org, scoped bindings |
| Model | Project, Environment (one namespace), App, Release, BuildRun | Domain CRD (the domain is a field on App), Service/Addon, backup |
| Deploy | From an image (external registry) + from git with Dockerfile/Railpack on buildkitd; rollback to the previous release | CNB, review apps, promotion, cron |
| Networking | HTTPRoute on the Gateway installed by the installer + TLS via cert-manager; the `*.sslip.io` domain by default | Custom domain verification, basic auth |
| Realtime | SSE per tab (status + logs), terminal over WS | Metrics (replicas/status only), K8s events in the UI |
| Git | GitHub webhook + polling fallback | GitLab, Gitea, Bitbucket |
| Ops | `install.sh` for k3s, `kuben doctor`, `backup`/`restore` of SQLite + CRD export | HA, multi-cluster, notifications |
| UI | Login, projects, app detail (overview, deploys, logs, terminal, settings), command palette | Dark/light toggle (dark only), i18n (EN only, but RTL-ready) |

**MVP exit:** one person takes a Node/Go/Python repo from zero (an empty VPS) to a URL with TLS in 5 minutes, sees logs, opens a shell, gets the next push auto-deployed, performs a rollback, and after restarting Kuben nothing is broken. Idle RSS < 30MiB; image < 30MB. A 1-hour log soak test with no memory growth.

### 5.2 Revised day-one priorities

**D1-Structural (phase 0, non-negotiable):** the data boundary (2.2), cookie session + opaque token, projection, bounded LogHub, terminal backpressure, SSA + idempotent reconcile, BuildRun/Release as CRDs, the OpenAPI pipeline, bootstrap order, CRD self-apply, environment soft-delete, structured logging + request ID, `/livez`/`/readyz` health, graceful shutdown, budgets in CI.

**D1-Stub (a trait in phase 0, implemented later):** `IdentityProvider` (local now, OIDC later), `LeaderElector` (noop now), `PolicyEngine` (static now, Cedar later), `MetricsSource` (metrics-server now, kubelet later), `NotificationSink` (log now), `BlobStore` (file now, S3 later), `ClusterRegistry` (one cluster now).

**D2:** the rest of section 8 of the v1.0 document.

### 5.3 Revised roadmap

| Phase | Duration (3 people) | Output | Exit |
|---|---|---|---|
| **0 — Skeleton** | 6 weeks | 6 crates, local auth, two-DB store, CRDs + self-apply, OpenAPI → TS, UI shell, CI with budgets, a first `install.sh` | `kuben serve` on kind + login + an (empty) app list + green budgets |
| **1 — MVP** | 6 weeks (12 in total) | The table in 5.1 | The MVP exit above; **5 external alpha users** |
| **2 — Incumbent parity** | 16 to 20 weeks | Git providers, data services (CNPG, Valkey, MariaDB), review apps, promotion, notifications, cron, Trivy, custom domains, template catalog + importer from the incumbent, CLI, Helm, optional Zot, `db migrate`, docs (Astro), i18n | Migrating a real incumbent installation; public beta |
| **3 — Enterprise** | 12 to 16 weeks | OIDC/passkeys/scoped RBAC, HA (Postgres + leader + N API), multi-cluster, full quota/NetPol/PSA, air-gap, OTel, session recording, Merkle audit anchor, CNB integration, self-upgrade | Green chaos suite; a tested restore; 1.0 |
| **4 — Differentiators** | Ongoing | Compose import, MCP, Terraform, GitHub Action, two-way GitOps, scale-to-zero, Cedar | — |

**Total to 1.0: roughly 45 to 55 weeks.** Do not put this number in the README; put the milestones there instead.

### 5.4 Three spikes before phase 0 (two to three days each)

These are the high-risk assumptions; if one of them fails, the plan changes:

1. **Spike-A (RSS):** a binary with kube + axum + sqlx + rustls that watches 6 kinds with projection, on kind with 200 pods → measure idle RSS with three allocators. If it is > 40MiB, revise the budget.
2. **Spike-B (rootless BuildKit):** rootless buildkitd on default k3s plus one managed cluster (GKE Autopilot, for instance, which is strict). If it does not work on the managed one, remote build fallback moves into phase 1.
3. **Spike-C (terminal backpressure):** `yes` inside a container with a throttled browser → server memory usage must stay flat and Ctrl-C must take effect in under 500ms.

---

## 6. Added ADRs

| ADR | Title | Decision |
|---|---|---|
| 013 | One runtime in phase 0; bulkhead behind config | Replaces ADR-003 |
| 014 | One SSE per tab with the scope in the URL + WS for the terminal only | Transport rule |
| 015 | Data ownership: SQL ↔ CRD via `uid` and BindingGC | Replaces ADR-001 |
| 016 | A persistent buildkitd + a lightweight `buildctl` Job; external registry in the MVP | Replaces ADR-009 |
| 017 | CRD self-apply at boot + ratcheting + storage migration | CRD lifecycle |
| 018 | Environment soft-delete with a grace period and `deletionPolicy` | Deletion safety |
| 019 | Bootstrap order and polling fallback for webhooks | Real zero-ops |
| 020 | Append-only audit in the core; Merkle anchor in Enterprise | Replaces the hash chain |
| 021 | 6 crates in phase 0; the criterion for splitting | Structure |
| 022 | Threat model and a two-role RBAC in HA | Security |

---

## 7. Architecture definition of done, before the first line of code

- [ ] ADRs 001 through 022 are written and reviewed (each at most one page: context, decision, consequences).
- [ ] The three spikes from section 5.4 have been run and their results recorded in the corresponding ADR.
- [ ] The threat model (section 3.7) lives in `docs/security/threat-model.md` and every control is mapped to a negative E2E test.
- [ ] The bootstrap order (section 3.1) is written up as a runbook and has been executed by hand on an empty VPS.
- [ ] The initial OpenAPI schema for the MVP (endpoints only, no implementation) is written, and the frontend can start against an MSW mock.
- [ ] The `v1alpha1` CRDs are generated with CEL and a meaningful `kubectl explain`, and a sample `kubectl apply` works on kind.
- [ ] The budgets from section 13 of the v1.0 document are implemented as a GitHub Action gate (even if the binary is still hello world).
- [ ] A `CONTRIBUTING.md` with the six invariants (section 1.4 of the v1.0 document) as a review checklist.

---

## 8. Conclusion

v1.0 described a correct architecture, but it was "the architecture of a 10-person team with an 18-month budget". v1.1 keeps the same direction and makes it executable with these changes:

1. **Simpler:** one runtime, one SSE per tab, 6 crates, audit without a chain.
2. **More honest:** external registry in the MVP, Kuben = cluster-admin-lite, bootstrap via sslip.io, a 12-to-18-month schedule to 1.0.
3. **Safer:** environment soft-delete, secret references in Release, a 5-second session cache, an explicit threat model.
4. **Faster for the user:** buildkitd with a persistent cache — the one change a user migrating from Coolify feels immediately.
5. **Measurable:** three spikes before committing, a 12-week MVP with an explicit exit, and an architecture DoD.

If only one recommendation from this document is acted on: **build the MVP in section 5.1 with a closed scope and get 5 real users before a single line of phase 2 is written.** Even the best architecture in the world, left 18 months without users, ends up sharing the fate of unfinished rewrites.
