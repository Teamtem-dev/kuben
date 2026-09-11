# Kuben

**A Kubernetes-native PaaS in a single binary.** Deploy container images into isolated environments from a web UI or a typed REST API. Kuben turns them into Deployments, Services, autoscalers and Gateway API routes, and keeps them reconciled.

[![CI](https://img.shields.io/github/actions/workflow/status/Teamtem-dev/kuben/ci.yml?branch=main)](https://github.com/Teamtem-dev/kuben/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Teamtem-dev/kuben?include_prereleases&sort=semver)](https://github.com/Teamtem-dev/kuben/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

> **Status: alpha.** Image-based apps, environments, secrets, logs, teams, API tokens, releases and the release pipeline work end to end. With PostgreSQL, several replicas can run side by side (leader-elected controllers, rolling upgrades). Git builds, preview environments and scale-to-zero are on the roadmap.

## Highlights

- **One static binary** (Rust: axum, kube-rs, sqlx) with the React UI embedded. CI enforces a 25 MiB binary budget and a 200 kB JS budget.
- **Projects → environments → apps.** Every environment gets its own namespace with Pod Security Admission, a resource quota (no LoadBalancer/NodePort services), default limits and a network policy that isolates tenants from each other.
- **Apps from any image:** zero-downtime rolling updates with startup/readiness/liveness probes, CPU autoscaling, env vars, write-only secrets, logs and rolling restarts.
- **Day-2 operations built in:**
  - release history with one-click rollback
  - staging → production promotion with a diff preview
  - persistent volumes that survive app deletion
  - cron jobs with "run now"
  - custom domains with automatic HTTPS and a DNS check
- **One-click templates:** PostgreSQL, Redis, MariaDB, n8n, Uptime Kuma, Vaultwarden, Gitea and more. Passwords are generated into a secret, never into the app spec.
- **Teams and automation:** hierarchical roles, invitations with forced password change, and scoped API tokens (role cap, project or environment) for CI/CD.
- **Kubernetes is the source of truth.** Everything is a `kuben.dev/v1alpha1` custom resource, so `kubectl` and GitOps tools work alongside the UI. Production environments are soft-deleted with a 7-day grace period.
- **Scales out on PostgreSQL:** every replica serves the API, one holds the controller Lease, and login throttling is shared.
- **Secure by default:**
  - Argon2id passwords with throttled logins
  - opaque `HttpOnly` sessions (no JWT in the browser)
  - a CSRF guard and a strict CSP
  - per-org authorization on every call
  - a free, always-on audit log of every change
  - a live event stream filtered per tenant

See the [user guide](docs/guide.md) for ten everyday scenarios, from deploying out of GitHub Actions to promoting a release.

## Install

**Binary** (Linux or macOS; x86_64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/Teamtem-dev/kuben/main/install.sh | bash
```

The script checks every download against the release's `checksums.txt` (SHA-256) before installing. Options: `--version v0.1.0`, `--dir ~/.local/bin`, `--no-sudo`.

**Kubernetes** (Helm, Kubernetes ≥ 1.29):

```bash
helm install kuben oci://ghcr.io/teamtem-dev/charts/kuben --namespace kuben-system --create-namespace
```

**Container image:** `ghcr.io/teamtem-dev/kuben` (linux/amd64 and linux/arm64, distroless, non-root).

Every release artifact carries a build provenance attestation:

```bash
gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
```

See [docs/deploy.md](docs/deploy.md) for configuration, exposing apps through a Gateway, backups and upgrades.

## Development

You need Rust (stable, MSRV 1.94), Node ≥ 22.12 with corepack, and [just](https://github.com/casey/just). The e2e suite also needs `kind`.

```bash
just setup    # toolchain + JS dependencies
just dev      # API on :8080 + Vite on :5173
just ci       # everything CI gates on: fmt, clippy, tests, drift, web build and size
just e2e      # full end-to-end run against a kind cluster
just gen      # regenerate openapi.json, the TS client and the CRD manifests
```

| Path | What it is |
|---|---|
| `crates/kuben` | The binary: `serve`, `migrate`, `doctor`, `reset-admin`, `backup`, `restore` |
| `crates/kuben-api` | HTTP API (axum + utoipa), auth, SSE stream, embedded UI |
| `crates/kuben-platform` | Controllers, informers and projections, supervisor, health |
| `crates/kuben-store` | SQLite/PostgreSQL store (users, sessions, role bindings, audit) |
| `crates/kuben-crd` | `kuben.dev/v1alpha1` CRD types + `crdgen` |
| `crates/kuben-core` | Domain types, config, permissions, errors |
| `apps/web` | React 19 + TanStack Router/Query + Tailwind v4 |
| `packages/api-client` | Generated OpenAPI spec and TypeScript types (committed; CI fails on drift) |
| `charts/kuben` | Helm chart (CRDs generated from the Rust types) |

Architecture decisions are indexed in [docs/adr](docs/adr/README.md). The CI/CD pipeline and the release process are documented in [docs/ci-cd.md](docs/ci-cd.md). Contribution rules are in [CONTRIBUTING.md](CONTRIBUTING.md), and to report a vulnerability see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
