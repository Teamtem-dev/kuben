<div align="center">
  <a href="https://kuben.teamtem.com">
    <img alt="Kuben" src="apps/site/public/brand/kuben-logo.svg" height="128">
  </a>
  <h1>Kuben</h1>

  <p><strong>A Kubernetes PaaS in a single binary.</strong></p>

  <a href="https://teamtem.com"><img alt="Made by Teamtem" src="https://img.shields.io/badge/MADE%20BY%20TEAMTEM-000000.svg?style=for-the-badge&labelColor=000"></a>
  <a href="https://github.com/Teamtem-dev/kuben/releases"><img alt="Release" src="https://img.shields.io/github/v/release/Teamtem-dev/kuben?style=for-the-badge&labelColor=000&color=f68b12&label=release"></a>
  <a href="https://github.com/Teamtem-dev/kuben/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/Teamtem-dev/kuben/ci.yml?branch=main&style=for-the-badge&labelColor=000&label=ci"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-Apache--2.0-f68b12.svg?style=for-the-badge&labelColor=000"></a>
  <a href="https://kuben.teamtem.com/docs/"><img alt="Documentation" src="https://img.shields.io/badge/docs-kuben.teamtem.com-f68b12.svg?style=for-the-badge&labelColor=000"></a>
</div>

## Getting Started

Kuben turns any Kubernetes cluster into a platform your team can use. Give it a container image and it gives you an isolated environment with a public HTTPS address, zero-downtime rollouts, autoscaling, logs, release history with one-click rollback, and an audit log of who changed what. It creates the Deployments, Services, autoscalers and HTTPS routes, and keeps them in sync.

**One command on a fresh Linux server** (as root; verifies checksums before installing):

```bash
curl -fsSL https://kuben.teamtem.com/install.sh | sh
```

It installs k3s if there is no cluster, sets Kuben up as a service and prints the link to the setup page, where you create the admin account. Run it again to upgrade. On a workstation or without root it installs the binary only. See [Install the binary](https://kuben.teamtem.com/docs/getting-started/binary/).

**Helm** (Kubernetes 1.29 or later):

```bash
helm install kuben oci://ghcr.io/teamtem-dev/charts/kuben --namespace kuben-system --create-namespace
```

Read the generated admin password and open the console:

```bash
kubectl -n kuben-system get secret kuben-initial-admin -o jsonpath='{.data.password}' | base64 -d
kubectl -n kuben-system port-forward svc/kuben 8080:80
```

Browse to `http://localhost:8080` and sign in as `admin@kuben.local`. The [quickstart](https://kuben.teamtem.com/docs/getting-started/quickstart/) walks through it, and [Deploy your first app](https://kuben.teamtem.com/docs/getting-started/first-app/) takes it from there.

The image `ghcr.io/teamtem-dev/kuben` (amd64 and arm64, distroless, non-root) and archives for every platform are on the [releases page](https://github.com/Teamtem-dev/kuben/releases), each with a checksum and a build attestation.

## What you get

- **Projects → environments → apps.** Each environment is its own namespace with quotas and network policies.
- **Deploy any container image** with zero-downtime rollouts, health checks, autoscaling, environment variables and write-only secrets.
- **Day-2 operations built in:** logs, restarts, release history with rollback, promotion between environments with a diff preview, persistent volumes, cron jobs, and custom domains with automatic HTTPS.
- **One-click templates:** PostgreSQL, Redis, MariaDB, n8n, Uptime Kuma, Vaultwarden, Gitea and more, with generated credentials.
- **Teams and CI/CD:** four roles, invitations, scoped API tokens, and an audit log of every change.
- **GitOps-friendly:** everything is a Kubernetes custom resource, so `kubectl` and GitOps tools work alongside the UI.
- **Small and auditable:** one Rust binary of at most 26 MiB with the console embedded; the pod requests 64 MiB of memory.

## Documentation

Visit [kuben.teamtem.com/docs](https://kuben.teamtem.com/docs/) for the full documentation:

| | |
|---|---|
| [Quickstart](https://kuben.teamtem.com/docs/getting-started/quickstart/) | install, sign in, first steps |
| [Guides](https://kuben.teamtem.com/docs/guides/deploy-from-ci/) | deploying from CI, teams, rollbacks, volumes, templates, domains, promotion |
| [Operations](https://kuben.teamtem.com/docs/operations/production-install/) | production install, HTTPS, high availability, backups, upgrades, security model |
| [Reference](https://kuben.teamtem.com/docs/reference/cli/) | CLI, configuration, Helm values, custom resources, REST API |
| [Changelog](https://kuben.teamtem.com/changelog/) | every release |

The in-repository originals are in [`docs/`](docs), including the [architecture decision records](docs/adr/README.md).

## Roadmap

Git builds with BuildKit, preview environments for every pull request, scale-to-zero for idle apps, and SSO (OIDC). See the [roadmap](https://kuben.teamtem.com/docs/roadmap/) and [open a feature request](https://github.com/Teamtem-dev/kuben/issues/new?template=feature_request.yml) for what you need.

## Community

Questions, ideas and bug reports go to [GitHub Issues](https://github.com/Teamtem-dev/kuben/issues). Project news is on the [blog](https://kuben.teamtem.com/blog/). Teams running Kuben in production can [contact Teamtem](https://kuben.teamtem.com/enterprise/).

Our [Code of Conduct](CODE_OF_CONDUCT.md) applies to every Kuben community space.

## Contributing

Contributions are welcome. Before you open a pull request, read the [contribution guidelines](CONTRIBUTING.md): they cover the workflow and the invariants every change is reviewed against.

You need Rust (stable; MSRV 1.94) and [Bun](https://bun.com) 1.4 or later. [Turborepo](https://turborepo.dev) runs every task in both languages from one graph:

```bash
bun run setup   # toolchain, JS dependencies, cargo-nextest
bun run dev     # API on :8080, console on :5173
bun run ci      # everything CI checks
```

### Good first issues

Issues labelled [good first issue](https://github.com/Teamtem-dev/kuben/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) have a limited scope and are a good way to get to know the codebase.

## Security

If you find a security vulnerability in Kuben, please **report it privately** through [GitHub's private vulnerability reporting](https://github.com/Teamtem-dev/kuben/security/advisories/new) and do not open a public issue. See [SECURITY.md](SECURITY.md) for details and the [security model](https://kuben.teamtem.com/docs/operations/security/) for what Kuben protects against, and what it does not.

## License

[Apache-2.0](LICENSE). Built by [Teamtem](https://teamtem.com).
