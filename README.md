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

## What Kuben is

Kuben turns a Kubernetes cluster into a platform your team can use. Give it a container image or a GitHub repository, and it gives you an isolated environment with a public HTTPS address, zero-downtime rollouts, autoscaling, logs, release history with one-click rollback, and an audit log of who changed what. It creates the Deployments, Services, autoscalers and HTTPS routes, and keeps them in sync. Its state lives in PostgreSQL; everything it runs is plain Kubernetes that `kubectl` can read.

**Versions.** The stable line is **1.2.x** (written in Rust, maintained on the [`release/1.2`](https://github.com/Teamtem-dev/kuben/tree/release/1.2) branch). This branch is **Kuben 2.x**, the same product rewritten in Go; it ships as pre-releases (`2.0.0-alpha.N`, then beta and rc) until 2.0.0. The installers below pick the newest stable release unless you name a version.

## Getting started

**One command on a fresh Linux server** (as root; verifies checksums before installing):

```bash
curl -fsSL https://kuben.teamtem.com/install.sh | sh
```

It installs k3s if there is no cluster, PostgreSQL, a system user and `kuben.service`, and prints the link to the setup page, where you create the admin account. Run it again to upgrade. On a workstation or without root it installs the binary only. See [Install the binary](https://kuben.teamtem.com/docs/getting-started/binary/).

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

The image `ghcr.io/teamtem-dev/kuben` (amd64 and arm64, distroless, non-root) and archives for every platform are on the [releases page](https://github.com/Teamtem-dev/kuben/releases), with a signed checksum list, SBOMs and build attestations.

### Try a 2.0 pre-release

Pre-releases are never picked by default: name one explicitly, on a test installation first. Replace `v2.0.0-alpha.1` with the newest pre-release on the [releases page](https://github.com/Teamtem-dev/kuben/releases).

```bash
# the binary and kuben setup
curl -fsSL https://kuben.teamtem.com/install.sh | sh -s -- --version v2.0.0-alpha.1

# Helm
helm upgrade --install kuben oci://ghcr.io/teamtem-dev/charts/kuben -n kuben-system --create-namespace --version 2.0.0-alpha.1
```

2.0 keeps the database schema, the REST API, the custom resources and the configuration of 1.2, so going back to 1.2.x is a matter of installing the older version. Run `kuben upgrade-check` with the new binary first. See [Upgrading from 1.2 to 2.0](https://kuben.teamtem.com/docs/operations/upgrading/#upgrading-from-12-to-20).

## What you get

- **Projects → environments → apps.** Each environment is its own namespace with quotas and network policies.
- **Deploy any container image, or build from Git** through a GitHub App, in isolated BuildKit Jobs, with an SBOM and a vulnerability scan of every image and a policy that can block a deployment.
- **Zero-downtime rollouts**, health checks, autoscaling, environment variables and write-only secrets.
- **Day-2 operations built in:** logs, restarts, release history with rollback, promotion between environments with a diff preview, persistent volumes, cron jobs, custom domains with automatic HTTPS, and a Doctor that says why an address does not answer.
- **Preview environments** for every pull request, with a lifetime and no secrets for forks.
- **One-click templates:** PostgreSQL, Redis, MariaDB, n8n, Uptime Kuma, Vaultwarden, Gitea and more, with generated credentials.
- **Teams and CI/CD:** four roles, invitations, single sign-on (OpenID Connect), scoped API tokens, GitHub Actions sign-in without a stored token, and an audit log of every change.
- **Operations:** scheduled backups with a checked restore, an upgrade preflight, incidents with webhooks, and local support bundles.
- **Plain Kubernetes underneath:** projects, environments and apps are custom resources and everything they run is ordinary Kubernetes objects, so `kubectl` shows it all. Kuben's database is the source of truth, and it puts back what someone else changed.
- **Small and auditable:** one static binary with the console embedded. In the Helm chart the Kuben container requests 50m CPU and 64 MiB of memory; the chart's own PostgreSQL requests another 128 MiB.

## Documentation

Visit [kuben.teamtem.com/docs](https://kuben.teamtem.com/docs/) for the full documentation:

| | |
|---|---|
| [Quickstart](https://kuben.teamtem.com/docs/getting-started/quickstart/) | install, sign in, first steps |
| [Guides](https://kuben.teamtem.com/docs/guides/deploy-from-ci/) | deploying from CI, teams, rollbacks, previews, volumes, templates, domains, promotion |
| [Operations](https://kuben.teamtem.com/docs/operations/production-install/) | production install, HTTPS, high availability, backups, upgrades, runbooks, security model |
| [Reference](https://kuben.teamtem.com/docs/reference/cli/) | CLI, configuration, Helm values, custom resources, REST API |
| [Contributing](https://kuben.teamtem.com/docs/contributing/development/) | development setup, CI and releases, [architecture decisions](https://kuben.teamtem.com/docs/contributing/architecture-decisions/) |
| [Changelog](https://kuben.teamtem.com/changelog/) | every release |

In this repository, [ARCHITECTURE.md](ARCHITECTURE.md) maps the apps, the packages and the Go code, and [CONTRIBUTING.md](CONTRIBUTING.md) lists the rules every change is reviewed against.

## Roadmap

Git builds, preview environments and single sign-on already ship. Next are scale-to-zero for idle apps, terminals into running pods, and separate service accounts for the API and the controllers. See the [roadmap](https://kuben.teamtem.com/docs/roadmap/) and [open a feature request](https://github.com/Teamtem-dev/kuben/issues/new?template=feature_request.yml) for what you need.

## Community

Questions, ideas and bug reports go to [GitHub Issues](https://github.com/Teamtem-dev/kuben/issues). Project news is on the [blog](https://kuben.teamtem.com/blog/). Teams running Kuben in production can [contact Teamtem](https://kuben.teamtem.com/enterprise/).

Our [Code of Conduct](CODE_OF_CONDUCT.md) applies to every Kuben community space.

## Contributing

Contributions are welcome. Before you open a pull request, read the [contribution guidelines](CONTRIBUTING.md): they cover the workflow and the invariants every change is reviewed against.

You need [Go](https://go.dev) (the version in `go.work`), [Bun](https://bun.com) 1.4 or later, and a PostgreSQL for the server. [Turborepo](https://turborepo.dev) runs every task in both languages from one graph:

```bash
bun run setup   # checks for Go and Bun, installs the JS dependencies, downloads the Go modules
export KUBEN_DATABASE__URL=postgres://postgres:kuben@localhost:5432/postgres
bun run dev     # the API on :8080 (kuben serve --dev), the console on :5173
bun run ci      # what CI checks: Go and Biome checks, tests, drift, size, govulncheck
```

The [development setup](https://kuben.teamtem.com/docs/contributing/development/) has the details, including a PostgreSQL in one `docker run`.

### Good first issues

Issues labelled [good first issue](https://github.com/Teamtem-dev/kuben/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) have a limited scope and are a good way to get to know the codebase.

## Security

If you find a security vulnerability in Kuben, please **report it privately** through [GitHub's private vulnerability reporting](https://github.com/Teamtem-dev/kuben/security/advisories/new) and do not open a public issue. See [SECURITY.md](SECURITY.md) for the supported versions and how releases are verified, and the [security model](https://kuben.teamtem.com/docs/operations/security/) for what Kuben protects against, and what it does not.

## License

[Apache-2.0](LICENSE). Built by [Teamtem](https://teamtem.com).
