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
