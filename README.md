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
