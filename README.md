# Kuben

**A Kubernetes PaaS in a single binary.** Deploy container images into isolated environments from a web UI or a REST API. Kuben creates the Deployments, Services, autoscaling and HTTPS routes, and keeps them in sync.

[![CI](https://img.shields.io/github/actions/workflow/status/Teamtem-dev/kuben/ci.yml?branch=main)](https://github.com/Teamtem-dev/kuben/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Teamtem-dev/kuben?include_prereleases&sort=semver)](https://github.com/Teamtem-dev/kuben/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

> **v1.0.** Deploying images, environments, secrets, logs, teams, API tokens and releases work end to end, and several replicas can run side by side on PostgreSQL. Git builds, preview environments and scale-to-zero come next (see [Roadmap](#roadmap)).

## Features

- **Projects → environments → apps.** Each environment is isolated in its own namespace, with quotas and network policies.
- **Deploy any container image** with zero-downtime rollouts, health checks, autoscaling, environment variables and write-only secrets.
- **Day-2 operations built in:** logs, restarts, release history with one-click rollback, promotion between environments with a diff preview, persistent volumes, cron jobs, and custom domains with automatic HTTPS.
- **One-click templates:** PostgreSQL, Redis, MariaDB, n8n, Uptime Kuma, Vaultwarden, Gitea and more.
- **Teams and CI/CD:** roles, invitations, scoped API tokens, and an audit log of every change.
- **GitOps-friendly:** everything is a Kubernetes custom resource, so `kubectl` and GitOps tools work alongside the UI.

## Quick start

You need Kubernetes 1.29 or later and Helm.

```bash
helm install kuben oci://ghcr.io/teamtem-dev/charts/kuben --namespace kuben-system --create-namespace
```

Read the generated admin password:

```bash
kubectl -n kuben-system get secret kuben-initial-admin -o jsonpath='{.data.password}' | base64 -d
```

Open the UI:

```bash
kubectl -n kuben-system port-forward svc/kuben 8080:80
```

Then browse to <http://localhost:8080> and sign in as `admin@kuben.local`. To publish apps on your own domain with HTTPS, follow [docs/deploy.md](docs/deploy.md).

Other ways to install:

- **Binary** (Linux or macOS, x86_64 or arm64). The script checks every download against the release checksums.
  ```bash
  curl -fsSL https://raw.githubusercontent.com/Teamtem-dev/kuben/main/install.sh | bash
  ```
  Then run `kuben doctor`. The binary needs a cluster to manage (for example k3s, via `KUBECONFIG`); see [running the binary](docs/deploy.md#running-the-binary-on-a-server).
- **Container image:** `ghcr.io/teamtem-dev/kuben` (amd64 and arm64, distroless, non-root).

## Roadmap

- Git builds with BuildKit, without an external CI
- Preview environments for every pull request
- Scale-to-zero for idle apps
- SSO (OIDC)

Missing something? [Open a feature request](https://github.com/Teamtem-dev/kuben/issues/new?template=feature_request.yml).

## Documentation

The website, guides, reference and blog live at [kuben.teamtem.com](https://kuben.teamtem.com) (source: [`apps/site`](apps/site)). The documents below are the in-repository originals.

| Document | Covers |
|---|---|
| [User guide](docs/guide.md) | ten everyday scenarios, from deploying from GitHub Actions to promoting a release |
| [Deployment](docs/deploy.md) | configuration, domains and HTTPS, multiple replicas, backups, upgrades, security model |
| [CI/CD and releases](docs/ci-cd.md) | what CI checks and how releases are published |
| [Architecture decisions](docs/adr/README.md) | why things are built the way they are |

## Development

You need Rust (stable; MSRV 1.94) and [Bun](https://bun.com) 1.4 or later. [Turborepo](https://turborepo.dev) runs every task in both languages from one graph. The end-to-end tests also need `kind`.

```bash
bun run setup   # toolchain, JS dependencies, cargo-nextest
bun run dev     # API on :8080, UI on :5173
bun run ci      # everything CI checks
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request, and see [SECURITY.md](SECURITY.md) to report a vulnerability. Participation follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[Apache-2.0](LICENSE)
