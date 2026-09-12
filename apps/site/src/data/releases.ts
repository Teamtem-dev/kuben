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
