// Kuben releases for /changelog/. Dates are the tag dates; the full notes
// live on GitHub. Add an entry when a release is cut.
export type Release = {
  version: string
  date: string
  title: string
  prerelease?: boolean
  highlights: string[]
}

export const releases: Release[] = [
  // ─── NOT LIVE: goes live with the v2.0.0-alpha.1 tag ────────────────────
  // This file has no "unreleased" state: an entry in the array is on the
  // site as soon as it is merged. Uncomment the entry below in the commit
  // that is tagged v2.0.0-alpha.1, and set `date` to the tag's date.
  //
  // {
  //   version: 'v2.0.0-alpha.1',
  //   date: 'YYYY-MM-DD',
  //   title: 'Kuben in Go: the first 2.0 pre-release',
  //   prerelease: true,
  //   highlights: [
  //     'The server, the kuben CLI and the cluster agent are rewritten in Go. It is not a performance rewrite: it brings the maintained Kubernetes libraries (client-go, controller-runtime, envtest), builds in seconds and code more people can read.',
  //     'Same contracts as 1.2: the REST API is generated from the same OpenAPI document, and an oracle test compares its answers with the released 1.2.0 binary; the CRD manifest, the database schema, the configuration keys, the Helm values and the AgentLink protocol are unchanged. 2.0.0 adds no migration, so going back to 1.2.x is a change of binary or image.',
  //     'CLI parity: every kuben command and flag of 1.2, with the same output of --version and exit status 2 for a command line that does not parse.',
  //     'A new console on shadcn/ui: a home dashboard, tables with filters and pages, the Doctor with its evidence graph, and settings for single sign-on and CI trust, in English and Persian, light and dark, under the same strict content security policy.',
  //     'Release files keep their 1.x names and signatures: install.sh and the Helm chart work unchanged. Pre-releases are never latest: name the version (install.sh --version v2.0.0-alpha.1, helm --version 2.0.0-alpha.1), run kuben upgrade-check --major first, and see Upgrading from 1.2 to 2.0 for the Helm pre-upgrade check.',
  //     'Not in yet: scale-to-zero (the activator role is ignored with a warning), terminals, and a console view for builds. [runtime] settings are accepted and have no effect. 1.2.x remains the stable line.',
  //   ],
  // },
  // ────────────────────────────────────────────────────────────────────────
  // Ships with the v1.2.1 tag: the type has no "not yet released" state, so
  // this entry is live on the site as soon as it is merged.
  {
    version: 'v1.2.1',
    date: '2026-09-26',
    title: 'Scan reports parse again, and idle mode defaults to off under Helm',
    highlights: [
      'The image scan wrote a control character where the Trivy database date belongs, so no report parsed and every scan was recorded as unavailable: the scan gate never saw a finding. Reports parse again, with the date.',
      'The CRD manifest quotes off: kubectl and Helm read a plain off as the boolean false, so apps installed through the Helm chart got false as the default idle mode instead of off.',
    ],
  },
  {
    version: 'v1.2.0',
    date: '2026-09-17',
    title: 'PostgreSQL, Git builds and the supported MVP',
    highlights: [
      'Breaking: Kuben keeps its data in PostgreSQL. SQLite is gone and 1.x data is not imported; without a database URL the server stops with a message that shows how to run a local PostgreSQL.',
      'Deploy an image to an app and expose it over HTTPS; the cluster agent connects to Kuben over mutual TLS (AgentLink).',
      'Git sources through a GitHub App, built in isolated in-cluster BuildKit Jobs.',
      'Organizations with roles, kuben backup and restore, managed secrets, single sign-on, CI trust for GitHub Actions, and image scans with a gate on deploys.',
      'Preview environments, custom domains with claims and DNS-01 certificates (kuben dns01-issuer), and status pages.',
    ],
  },
  {
    version: 'v1.1.2',
    date: '2026-09-13',
    title: 'Ready within seconds on a cluster that has just started',
    highlights: [
      'One failing watch no longer restarts every informer: each kind retries on its own, so the errors of a freshly installed k3s (a CRD not there yet) cost seconds, not minutes of an unready server.',
      'Informers that have not listed within 45 seconds start over and log which kinds they were waiting for; a subsystem that ran for a minute before it failed restarts at the shortest delay again.',
      '/livez and /readyz are no longer logged on every poll, so the service log shows what matters; kuben setup prints the service’s latest warnings when it is not ready yet.',
    ],
  },
  {
    version: 'v1.1.1',
    date: '2026-09-13',
    title: 'Setup that works around a busy port',
    highlights: [
      'When port 3000 is taken (the Dokploy console, for one), kuben setup names who holds it and asks which port to use, with the next free one filled in; without a terminal it takes that port. --port now also moves the port of an existing install.',
      'The config is written after the port is settled, so a run that stopped on a busy port no longer keeps failing.',
      'A warning when another web server holds ports 80 and 443, which apps need for public addresses.',
      'An orange KUBEN wordmark to open the install, orange spinners and prompts, ✔ for every step that is in place, and the time long steps took.',
    ],
  },
  {
    version: 'v1.1.0',
    date: '2026-09-13',
    title: 'One command, one server',
    highlights: [
      'curl -fsSL https://kuben.teamtem.com/install.sh | sh on a fresh Linux server installs k3s if needed, the service user, the configuration, kuben.service and the firewall, and prints the link to the setup page; the same command upgrades (kuben setup, status, uninstall).',
      'The first admin account is created from the console on a setup page, guarded by a token from the installer; nothing is seeded and nothing is logged.',
      'The session cookie is Secure exactly when the console has an https public URL (cookie_secure = auto), so signing in over http://<ip>:3000 works on day one.',
      'Installer output in the style of modern CLIs: one line per step, with a spinner while it runs.',
      'The smoke test runs the one-liner as root on fresh runners after every release and every day: setup page, an app, an upgrade in place, uninstall.',
    ],
  },
  {
    version: 'v1.0.4',
    date: '2026-09-12',
    title: 'The Helm chart installs with default values',
    highlights: [
      'Charts 1.0.0 to 1.0.3 rendered KubenConfig.spec as null without platform settings, and the API server rejected the documented helm install; the chart now renders an empty spec.',
      'CI validates the chart against a real API server before merge, and smoke-test failures report the failing command’s output.',
    ],
  },
  {
    version: 'v1.0.3',
    date: '2026-09-12',
    title: 'A first run that works without root',
    highlights: [
      'The binary keeps its database in ~/.local/state/kuben or systemd’s state directory, so kuben doctor and kuben serve work without root; an existing /data is still used.',
      'A generated admin password is never logged: the binary prints it to its terminal, or writes an owner-only file under systemd.',
      'kuben doctor reports an unreadable kubeconfig, and doctor and serve warn when the Secure cookie meets plain HTTP.',
      'The chart keeps the KubenConfig on uninstall, so apps keep their routes; the uninstall guide lists what stays behind.',
      'A smoke test installs the published binary, chart and image anonymously on k3s after every release and every day.',
    ],
  },
  {
    version: 'v1.0.3-rc.1',
    date: '2026-09-12',
    title: 'Security CI and dependency upgrades',
    prerelease: true,
    highlights: [
      'Workflows audited with zizmor; RustSec advisories and the published image re-checked every day.',
      'Release binaries embed their dependency list (cargo-auditable) and are scanned with Trivy before they ship.',
      'base64 0.23, rand 0.10, sha2 0.11 and tower-http 0.7.',
      'The web console moved to apps/console.',
    ],
  },
  {
    version: 'v1.0.2',
    date: '2026-09-12',
    title: 'One task runner for Rust and TypeScript',
    highlights: [
      'Turborepo runs every task through a native Cargo workspace; Bun replaces pnpm and Node (ADR-024).',
      'CI jobs call the same turbo tasks developers run locally.',
    ],
  },
  {
    version: 'v1.0.1',
    date: '2026-09-12',
    title: 'SQLite first-run fix',
    highlights: ['The binary creates the SQLite data directory on first start.'],
  },
  {
    version: 'v1.0.0',
    date: '2026-09-12',
    title: 'General availability',
    highlights: [
      'Several replicas on PostgreSQL: every replica serves the API, the Lease holder runs the controllers (ADR-023).',
      'Pods report ready only after the informers have synced, so a fresh replica never answers 404 for objects that exist.',
      'Safe concurrent bootstrap when several replicas start against a fresh database.',
      'Tested on Linux x64 and arm64, macOS and Windows on every change.',
    ],
  },
  {
    version: 'v1.0.0-rc.1',
    date: '2026-09-10',
    title: 'Production hardening',
    prerelease: true,
    highlights: [
      'Informer readiness gates and locked bootstrap for concurrent replicas.',
      'End-to-end suite on a real kind cluster.',
    ],
  },
  {
    version: 'v1.0.0-beta.1',
    date: '2026-09-06',
    title: 'Leader election',
    prerelease: true,
    highlights: [
      'Lease-based leader election for the controllers; standby replicas keep serving the API and the console.',
    ],
  },
  {
    version: 'v1.0.0-alpha.1',
    date: '2026-09-05',
    title: 'Architecture and invariants',
    prerelease: true,
    highlights: [
      'The architecture blueprint, the review of its trade-offs and the invariants every change is checked against.',
    ],
  },
  {
    version: 'v0.9.0',
    date: '2026-08-31',
    title: 'Helm chart, image and installer',
    highlights: [
      'Helm chart published as an OCI artifact.',
      'Multi-arch distroless image and a static musl binary.',
      'One-line installer that verifies checksums.',
    ],
  },
  {
    version: 'v0.8.0',
    date: '2026-08-18',
    title: 'Web console',
    highlights: [
      'React console with releases, rollbacks, logs and the audit log.',
      'Embedded into the binary with the embed-ui feature.',
    ],
  },
  {
    version: 'v0.7.0',
    date: '2026-07-31',
    title: 'Identity, roles and audit',
    highlights: [
      'Argon2id passwords, opaque cookie sessions and scoped API tokens.',
      'Login throttling, the append-only audit log and kuben doctor.',
    ],
  },
  {
    version: 'v0.6.0',
    date: '2026-07-15',
    title: 'REST API',
    highlights: [
      'Axum REST API with server-sent events, an OpenAPI document and a generated TypeScript client.',
    ],
  },
  {
    version: 'v0.5.0',
    date: '2026-06-28',
    title: 'Gateway API routing',
    highlights: ['HTTPRoutes for every web app and health probes for zero-downtime rollouts.'],
  },
  {
    version: 'v0.4.0',
    date: '2026-06-10',
    title: 'Controllers',
    highlights: [
      'Reconcilers turn apps into Deployments, Services, autoscalers and volumes and keep them in sync.',
    ],
  },
  {
    version: 'v0.3.0',
    date: '2026-05-20',
    title: 'Storage',
    highlights: [
      'SQLite and PostgreSQL behind one store, with embedded migrations and a test matrix across both.',
    ],
  },
  {
    version: 'v0.2.0',
    date: '2026-04-30',
    title: 'Custom resources',
    highlights: ['Project, Environment, App and Release custom resources and the domain core.'],
  },
  {
    version: 'v0.1.0',
    date: '2026-04-20',
    title: 'Foundation',
    highlights: ['The Cargo workspace, its crates and the build pipeline.'],
  },
]
