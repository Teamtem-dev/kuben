import { existsSync, readFileSync, statSync } from 'node:fs'
import { extname, join, normalize } from 'node:path'
import type { Page, Route } from '@playwright/test'

const dist = join(import.meta.dirname, '..', 'dist')

/** The content security policy Kuben serves the console with, from its source. */
function kubenCsp(): string {
  const file = '../../kuben/internal/httpapi/web/web.go'
  const source = readFileSync(join(import.meta.dirname, file), 'utf8')
  // `const CSP = "…" +\n\t"…"`: the Go string literals, concatenated.
  const decl = /const CSP = ((?:"[^"]*"\s*\+\s*)*"[^"]*")/.exec(source)?.[1]
  if (!decl) throw new Error(`CSP not found in ${file}`)
  return [...decl.matchAll(/"([^"]*)"/g)].map((m) => m[1]).join('')
}

const csp = kubenCsp()
const types: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.svg': 'image/svg+xml',
  '.woff2': 'font/woff2',
}

/** The build, as Kuben's fallback serves it: files, else `index.html`. */
async function serveConsole(route: Route) {
  const path = normalize(decodeURIComponent(new URL(route.request().url()).pathname)).replace(
    /^(\.\.[/\\])+/,
    '',
  )
  const file = join(dist, path)
  const found = file.startsWith(dist) && existsSync(file) && statSync(file).isFile()
  const served = found ? file : join(dist, 'index.html')
  await route.fulfill({
    status: 200,
    contentType: types[extname(served)] ?? 'application/octet-stream',
    headers: { 'content-security-policy': csp },
    body: readFileSync(served),
  })
}

/** The mocked data's "now"; visual tests pin the page's clock to it. */
export const now = Date.UTC(2026, 8, 16, 10, 0, 0)

export const user = {
  id: '0190f3c6-0000-7000-8000-000000000001',
  email: 'owner@example.com',
  display_name: 'Owner',
  via: 'session',
  must_change_password: false,
}

export const projects = [
  {
    name: 'shop',
    uid: '0190f3c6-0000-7000-8000-0000000000p1',
    display_name: 'Shop',
    description: null,
    org: null,
    environments: 1,
    ready: true,
    deleting: false,
    created_at: '2026-09-16T08:00:00Z',
  },
]

export const environment = {
  name: 'prod',
  resource_name: 'shop-prod',
  project: 'shop',
  env_type: 'production',
  namespace: 'kb-shop-prod',
  phase: 'Active',
  ready: true,
  message: null,
  deleting: false,
  deletion_scheduled_at: null,
  created_at: '2026-09-16T08:30:00Z',
}

export const tokens = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000c1',
    name: 'github-actions',
    prefix: 'kbn_pat_0190f3c6',
    role: 'developer',
    project: 'shop',
    environment: null,
    expires_at: now + 90 * 86_400_000,
    last_used_at: now - 3_600_000,
    revoked: false,
    created_at: now - 86_400_000,
  },
]

const process = (name: string, schedule: string | null = null) => ({
  name,
  command: [],
  port: schedule ? null : 8080,
  size: 'small',
  min_replicas: 1,
  max_replicas: 1,
  schedule,
  protocol: 'http',
})

export const app = {
  name: 'web',
  project: 'shop',
  environment: 'prod',
  namespace: 'kb-shop-prod',
  image: 'ghcr.io/acme/web:1.4.2',
  git_repo: null,
  url: 'https://web.apps.example.com',
  ready: true,
  reason: 'Available',
  message: null,
  processes: [process('web'), process('worker')],
  env: [],
  domains: ['web.apps.example.com'],
  volumes: [],
  created_at: '2026-09-16T09:00:00Z',
  exposure: {
    routed: true,
    message: null,
    hosts: [
      { host: 'web.apps.example.com', tls: 'auto', certificate_ready: true, certificate_message: null },
    ],
  },
}

const pod = (name: string, processName: string) => ({
  name,
  process: processName,
  phase: 'running',
  ready: true,
  restarts: 0,
  reason: null,
  node: 'node-1',
  started_at: '2026-09-16T09:01:00Z',
})

const phases = (list: string[]) => list.map((phase, i) => ({ phase, at: now - (list.length - i) * 4_000 }))

export const deployments = [
  {
    run: '0190f3c6-0000-7000-8000-00000000000b',
    generation: 3,
    reason: 'deploy',
    phase: 'succeeded',
    outcome: 'succeeded',
    requested_by: 'owner@example.com',
    created_at: now - 20_000,
    image: 'ghcr.io/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',
    timeline: phases(['planned', 'acceptedByCluster', 'applying', 'verifying', 'succeeded']),
  },
  {
    run: '0190f3c6-0000-7000-8000-00000000000a',
    generation: 2,
    reason: 'rollback',
    phase: 'failed',
    outcome: 'failed',
    requested_by: 'owner@example.com',
    created_at: now - 3_600_000,
    image: null,
    timeline: phases(['planned', 'applying', 'failed']),
  },
]

/** A run waiting for two approvals (the `approval` option of `mockApi`). */
export const awaitingRun = {
  run: '0190f3c6-0000-7000-8000-00000000000c',
  generation: 4,
  reason: 'deploy',
  phase: 'awaitingApproval',
  outcome: null,
  requested_by: 'carol@example.com',
  created_at: now - 10_000,
  image: 'ghcr.io/acme/web@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210',
  timeline: phases(['planned', 'awaitingApproval']),
}

export const planHash = '9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08'

export const approval = {
  run: awaitingRun.run,
  phase: 'awaitingApproval',
  requestedBy: 'carol@example.com',
  required: 2,
  approved: 1,
  expiresAt: now + 86_400_000,
  planHash,
  canDecide: true,
  decisions: [
    {
      approver: 'alice@example.com',
      decision: 'approved',
      comment: 'checked the migration',
      decidedAt: now - 5_000,
    },
  ],
}

const deploymentRun = {
  run: awaitingRun.run,
  operation: '0190f3c6-0000-7000-8000-0000000000o1',
  generation: 4,
  phase: 'awaitingApproval',
  approvals_required: 2,
  approval_expires_at: approval.expiresAt,
  plan_hash: planHash,
  warnings: ['the pods may not fit on the nodes'],
}

/** The Git source of the app with the `gitSource` option. */
export const gitRepo = 'https://github.com/acme/web'

const build = (id: string, fields: Record<string, unknown>) => ({
  id,
  attempt: 1,
  repository: 'acme/web',
  branch: 'main',
  commit: '0123456789abcdef0123456789abcdef01234567',
  strategy: 'railpack',
  phase: 'succeeded',
  blockedReason: null,
  failure: null,
  failureDetail: null,
  image: null,
  release: null,
  deployment: null,
  deployDecision: null,
  cancelRequested: false,
  createdAt: now - 600_000,
  startedAt: now - 590_000,
  finishedAt: now - 500_000,
  ...fields,
})

export const builtDigest = 'sha256:1111111111111111111111111111111111111111111111111111111111111111'

export const builds = [
  build('0190f3c6-0000-7000-8000-0000000000d3', {
    commit: 'aaaaaaa89abcdef0123456789abcdef01234567',
    phase: 'running',
    createdAt: now - 60_000,
    startedAt: now - 50_000,
    finishedAt: null,
    stages: [
      { name: 'clone', status: 'succeeded', startedAt: now - 50_000, finishedAt: now - 48_000 },
      { name: 'plan', status: 'succeeded', startedAt: now - 48_000, finishedAt: now - 45_000 },
      { name: 'build', status: 'running', startedAt: now - 45_000, finishedAt: null, detail: 'railpack' },
      { name: 'push', status: 'pending' },
    ],
  }),
  build('0190f3c6-0000-7000-8000-0000000000d2', {
    commit: 'bbbbbbb89abcdef0123456789abcdef01234567',
    phase: 'failed',
    failure: 'OutOfMemory',
    failureDetail: 'the build used more than 4 GiB',
    createdAt: now - 3_600_000,
    startedAt: now - 3_590_000,
    finishedAt: now - 3_500_000,
  }),
  build('0190f3c6-0000-7000-8000-0000000000d1', {
    image: `ghcr.io/acme/web@${builtDigest}`,
    release: '0190f3c6-0000-7000-8000-0000000000r1',
    deployment: deployments[0]?.run,
    deployDecision: 'deployed',
    createdAt: now - 86_400_000,
    startedAt: now - 86_390_000,
    finishedAt: now - 86_300_000,
    stages: [
      { name: 'clone', status: 'succeeded', startedAt: now - 86_390_000, finishedAt: now - 86_385_000 },
      { name: 'build', status: 'succeeded', startedAt: now - 86_385_000, finishedAt: now - 86_320_000 },
      { name: 'scan', status: 'skipped' },
      { name: 'push', status: 'succeeded', startedAt: now - 86_320_000, finishedAt: now - 86_300_000 },
    ],
  }),
]

/** The kept log of a settled build (text) and the followed log of a running one (lines). */
export const buildLog = '#1 [internal] load build definition\n#2 railpack plan: node 22\n#3 DONE 42.1s\n'

export const buildLogLines = ['#1 cloning acme/web@aaaaaaa', '#2 installing dependencies']

// ---- integrations (2.1) ----

export const gitConnections = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000g1',
    name: 'gitlab-acme',
    provider: 'gitlab',
    baseUrl: 'https://gitlab.com',
    authKind: 'token',
    username: 'acme-bot',
    defaultBranch: 'main',
    hasToken: true,
    tokenHint: 'x9Qa',
    webhookUrl: '/api/v1/webhooks/gitlab/0190f3c6-0000-7000-8000-0000000000g1',
    createdAt: now - 86_400_000,
    updatedAt: now - 86_400_000,
    lastCheckedAt: now - 3_600_000,
    lastError: null,
  },
  {
    id: '0190f3c6-0000-7000-8000-0000000000g2',
    name: 'codeberg',
    provider: 'gitea',
    baseUrl: 'https://codeberg.org',
    authKind: 'token',
    username: null,
    defaultBranch: null,
    hasToken: true,
    tokenHint: 'b7Zk',
    webhookUrl: '/api/v1/webhooks/gitea/0190f3c6-0000-7000-8000-0000000000g2',
    createdAt: now - 7_200_000,
    updatedAt: now - 7_200_000,
    lastCheckedAt: now - 600_000,
    lastError: 'the token was revoked (401)',
  },
]

/** The webhook secrets a new connection, and a rotation, show once. */
export const webhookSecret = 'whsec_created_0123456789abcdef'
export const rotatedWebhookSecret = 'whsec_rotated_fedcba9876543210'

export const connectionCheck = {
  ok: true,
  username: 'acme-bot',
  scopes: ['read_api', 'read_repository'],
  missingScopes: [],
  error: null,
  checkedAt: now,
}

const repository = (fullName: string, fields: Record<string, unknown> = {}) => ({
  id: fullName,
  fullName,
  description: null,
  defaultBranch: 'main',
  private: false,
  webUrl: `https://gitlab.com/${fullName}`,
  updatedAt: now - 86_400_000,
  ...fields,
})

export const repositoryPages = [
  {
    repositories: [
      repository('acme/web', { description: 'The shop front', private: true }),
      repository('acme/api'),
    ],
    page: 1,
    hasMore: true,
  },
  { repositories: [repository('acme/docs', { defaultBranch: 'trunk' })], page: 2, hasMore: false },
]

export const gitBranches = [
  { name: 'develop', commit: 'cccccccc89abcdef', protected: false, default: false },
  { name: 'main', commit: '0123456789abcdef', protected: true, default: true },
]

export const installations = [{ installationId: 4242, account: 'acme', suspended: false }]

export const registryPresets = [
  {
    id: 'dockerhub',
    label: 'Docker Hub',
    server: 'docker.io',
    usernameHint: 'Your Docker ID',
    passwordHint: 'A personal access token (read-only)',
    docsUrl: 'https://docs.docker.com/security/access-tokens/',
  },
  {
    id: 'ghcr',
    label: 'GitHub Packages',
    server: 'ghcr.io',
    usernameHint: 'Your GitHub user name',
    passwordHint: 'A classic token with read:packages',
    docsUrl: null,
  },
  {
    id: 'gitlab',
    label: 'GitLab',
    server: 'registry.gitlab.com',
    usernameHint: 'A deploy token user',
    passwordHint: 'The deploy token (read_registry)',
    docsUrl: null,
  },
  {
    id: 'quay',
    label: 'Quay',
    server: 'quay.io',
    usernameHint: 'A robot account',
    passwordHint: 'The robot token',
    docsUrl: null,
  },
  {
    id: 'harbor',
    label: 'Harbor',
    server: null,
    usernameHint: 'A robot account (robot$…)',
    passwordHint: 'The robot secret',
    docsUrl: null,
  },
  {
    id: 'custom',
    label: 'Custom',
    server: null,
    usernameHint: 'The registry user',
    passwordHint: 'Its password or token',
    docsUrl: null,
  },
]

export const orgRegistries = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000h1',
    name: 'ghcr',
    preset: 'ghcr',
    server: 'ghcr.io',
    username: 'acme-bot',
    hasPassword: true,
    createdAt: now - 86_400_000,
    updatedAt: now - 86_400_000,
    lastCheckedAt: now - 3_600_000,
    lastError: null,
  },
]

/** The Git source of the app with the `gitSource` option: through the GitLab connection. */
export const appSource = {
  installationId: 0,
  repository: 'acme/web',
  branch: 'main',
  strategy: 'railpack',
  context: '',
  dockerfile: null,
  imageRepository: 'registry.example.com/acme/web',
  head: '0123456789abcdef0123456789abcdef01234567',
  syncOperation: null,
  provider: 'gitlab',
  connection: gitConnections[0]?.id,
}

const scans = {
  release: '0190f3c6-0000-7000-8000-0000000000r1',
  gate: 'pass',
  reasons: [],
  images: [
    {
      digest: builtDigest,
      sbom: true,
      scan: {
        status: 'ok',
        scanner: 'trivy',
        databaseUpdatedAt: new Date(now - 86_400_000).toISOString(),
        scannedAt: new Date(now - 3_600_000).toISOString(),
        critical: 0,
        high: 1,
        medium: 3,
        low: 7,
        unknown: 0,
        findings: ['high:CVE-2026-0001'],
      },
    },
  ],
}

export const doctor = {
  status: 'fail',
  checks: [
    {
      id: 'gateway-class',
      subject: 'traefik',
      status: 'ok',
      detail: 'accepted by its controller',
      hint: null,
    },
    {
      id: 'gateway',
      subject: 'kuben-system/kuben',
      status: 'ok',
      detail: 'programmed (203.0.113.7)',
      hint: null,
    },
    { id: 'issuer', subject: 'letsencrypt', status: 'ok', detail: 'ready', hint: null },
    { id: 'port-80', subject: '80', status: 'ok', detail: 'the Gateway accepts connections', hint: null },
    {
      id: 'port-443',
      subject: '443',
      status: 'unknown',
      detail: 'the Gateway reports no address to try',
      hint: null,
    },
    { id: 'route', subject: '', status: 'ok', detail: 'accepted by the Gateway', hint: null },
    {
      id: 'certificate',
      subject: 'web.apps.example.com',
      status: 'warn',
      detail: 'served over plain HTTP',
      hint: 'set a ClusterIssuer',
    },
    {
      id: 'dns',
      subject: 'web.apps.example.com',
      status: 'fail',
      detail: 'no DNS record',
      hint: 'point the record at the Gateway',
    },
  ],
  graph: {
    nodes: [
      {
        layer: 'pods',
        status: 'ok',
        subject: 'Deployment web-web',
        evidence: ['2/2 ready'],
        observedAt: now,
        action: null,
      },
      { layer: 'gateway', status: 'ok', subject: 'kuben-system/kuben', evidence: [], action: null },
      {
        layer: 'dns',
        status: 'fail',
        subject: 'web.apps.example.com',
        evidence: ['web.apps.example.com: NXDOMAIN'],
        action: 'point the record at the Gateway',
      },
      {
        layer: 'tls',
        status: 'warn',
        subject: 'web.apps.example.com',
        evidence: ['no certificate yet'],
        action: null,
      },
    ],
    edges: [
      { from: 'dns', to: 'tls' },
      { from: 'gateway', to: 'tls' },
    ],
  },
  findings: [
    {
      kind: 'rootCause',
      layer: 'dns',
      status: 'fail',
      confidence: 'high',
      summary: 'no DNS record points at the Gateway',
      evidence: ['web.apps.example.com: NXDOMAIN'],
      related: ['tls'],
      action: 'point the record at the Gateway',
    },
  ],
}

const metrics = {
  window: '1h',
  available: true,
  reason: null,
  points: [0, 1, 2, 3].map((i) => ({
    at: `2026-09-16T09:5${i}:00Z`,
    cpuMillis: 120 + i * 15,
    memoryBytes: (96 + i * 4) * 1024 * 1024,
    pods: 2,
  })),
}

const iso = (ms: number) => new Date(ms).toISOString()

export const members = [
  {
    id: user.id,
    email: user.email,
    display_name: 'Owner',
    role: 'owner',
    must_change_password: false,
    active: true,
  },
  {
    id: '0190f3c6-0000-7000-8000-000000000002',
    email: 'carol@example.com',
    display_name: null,
    role: 'developer',
    must_change_password: true,
    active: true,
  },
]

export const incidents = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000e1',
    kind: 'deployment.failed',
    severity: 'critical',
    title: 'web in shop/prod failed to deploy',
    detail: 'the new pods never became ready',
    project: 'shop',
    environment: 'prod',
    app: 'web',
    openedAt: iso(now - 3_600_000),
    lastSeenAt: iso(now - 600_000),
    occurrences: 3,
    acknowledgedAt: null,
    acknowledgedBy: null,
    resolvedAt: null,
    resolvedBy: null,
    runbook: 'https://runbooks.example.com/deploy-failed',
  },
]

export const webhooks = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000w1',
    name: 'ops-pager',
    url: 'https://hooks.example.com/kuben',
    events: ['deployment.failed', 'incident.opened'],
    createdBy: user.email,
    createdAt: iso(now - 86_400_000),
    disabledAt: null,
    failures: 1,
    secret: null,
  },
]

export const deliveries = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000d1',
    event: 'deployment.failed',
    status: 'failed',
    attempts: 3,
    lastStatus: 502,
    lastError: 'bad gateway',
    createdAt: iso(now - 600_000),
    finishedAt: iso(now - 500_000),
  },
]

export const claims = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000f1',
    domain: 'example.com',
    status: 'pending',
    challengeName: '_kuben-challenge.example.com',
    challengeValue: 'kuben-verify=4f1d2c',
    method: null,
    createdBy: user.email,
    createdAt: iso(now - 86_400_000),
    verifiedAt: null,
    lastCheckedAt: null,
    lastError: null,
  },
]

export const audit = {
  events: [
    {
      seq: 2,
      id: '0190f3c6-0000-7000-8000-0000000000a2',
      at: now - 60_000,
      actor_kind: 'user',
      actor: user.email,
      action: 'app.update',
      target_kind: 'app',
      target: 'shop/prod/web',
      outcome: 'success',
      status: 200,
      ip: '203.0.113.9',
      request_id: null,
    },
    {
      seq: 1,
      id: '0190f3c6-0000-7000-8000-0000000000a1',
      at: now - 120_000,
      actor_kind: 'user',
      actor: user.email,
      action: 'startDeployment',
      target_kind: 'app',
      target: 'shop/prod/web',
      outcome: 'accepted',
      status: null,
      ip: '203.0.113.9',
      request_id: null,
    },
  ],
  next_before: null,
}

export const owner = {
  owner: 'shop-team',
  contact: '#shop-oncall',
  runbookUrl: 'https://runbooks.example.com/shop',
}

export const freezes = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000f9',
    reason: 'end-of-quarter close',
    app: null,
    createdBy: user.email,
    startsAt: iso(now - 3_600_000),
    endsAt: iso(now + 86_400_000),
    liftedAt: null,
    active: true,
  },
]

export const ciPolicies = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000c9',
    name: 'shop-deploy',
    project: '0190f3c6-0000-7000-8000-0000000000p1',
    environment: null,
    repository: 'acme/shop',
    repositoryId: 123456,
    repositoryOwnerId: 7890,
    refs: ['refs/heads/main'],
    environments: [],
    events: [],
    role: 'developer',
    tokenTtlSecs: 900,
    createdBy: user.id,
    createdAt: now - 86_400_000,
    revokedAt: null,
  },
]

export const health = {
  ready: true,
  database: 'postgres',
  cluster: true,
  seq: 42,
  pods: 2,
  subsystems: {
    agentlink: { state: 'ok', updated_at_ms: now },
    projections: { state: 'ok', updated_at_ms: now },
    webhooks: { state: 'degraded', last_error: 'hooks.example.com: 502', updated_at_ms: now },
  },
}

export const previews = [
  {
    environment: 'pr-42',
    repository: 'acme/shop',
    pullRequest: 42,
    epoch: 1,
    headRepository: 'acme/shop',
    branch: 'feature/cart',
    commit: '0123456789abcdef0123',
    trusted: true,
    state: 'active',
    autoDelete: true,
    expiresAt: iso(now + 20 * 3_600_000),
    remainingSeconds: 20 * 3_600,
    createdAt: iso(now - 4 * 3_600_000),
    closedAt: null,
    closeReason: null,
  },
]

const previewPolicy = {
  enabled: true,
  sourceEnvironment: 'prod',
  ttlHours: 24,
  maxActive: 5,
  allowForks: false,
  updatedBy: user.email,
  updatedAt: iso(now - 86_400_000),
}

const statusPage = {
  slug: 'shop',
  title: 'Shop status',
  enabled: true,
  environments: ['prod'],
  path: '/status/shop',
  updatedBy: user.email,
  updatedAt: iso(now - 86_400_000),
}

export const publicStatus = {
  title: 'Shop status',
  status: 'degraded',
  components: [
    { name: 'web', status: 'operational' },
    { name: 'worker', status: 'degraded' },
  ],
  incidents: [
    { component: 'worker', severity: 'warning', startedAt: iso(now - 3_600_000), resolvedAt: null },
  ],
  updatedAt: iso(now),
}

export const detached = [
  {
    id: '0190f3c6-0000-7000-8000-0000000000b1',
    app: 'legacy',
    namespace: 'kb-shop-prod',
    reason: 'moved to Helm',
    requestedBy: user.email,
    requestedAt: iso(now - 86_400_000),
    completedAt: iso(now - 86_000_000),
    releasedAt: null,
    releasedBy: null,
  },
]

const sse = (events: [string, unknown][]) =>
  events.map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join('')

/** Preferences as the console keeps them, set before it loads. */
export async function prefer(page: Page, locale: string, theme: string) {
  await page.addInitScript(
    (prefs) => window.localStorage.setItem('kuben.prefs', prefs),
    JSON.stringify({ locale, theme }),
  )
}

const json = (route: Route, body: unknown, status = 200) =>
  route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })

/**
 * Answer the console's API calls; `signedIn: false` shows the sign-in page,
 * `setupNeeded: true` the first-run setup. `approval` adds a run waiting for
 * approval (`decide`: the caller may decide; `watch`: they may not);
 * `gitSource` makes the app built from Git, with builds.
 */
export async function mockApi(
  page: Page,
  {
    signedIn = true,
    setupNeeded = false,
    approval: approvalMode,
    gitSource = false,
    buildDelta = false,
  }: {
    signedIn?: boolean
    setupNeeded?: boolean
    approval?: 'decide' | 'watch'
    gitSource?: boolean
    buildDelta?: boolean
  } = {},
) {
  const appPath = '/api/v1/projects/shop/environments/prod/apps/web'
  const runPath = `${appPath}/deployments/${awaitingRun.run}`
  const shownApproval = { ...approval, canDecide: approvalMode === 'decide' }
  let currentSource: Record<string, unknown> | null = gitSource ? (appSource as Record<string, unknown>) : null
  await page.route('http://kuben.test/**', serveConsole)
  await page.route('http://kuben.test/api/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/api/v1/setup') {
      return json(route, { needed: setupNeeded, token_required: false, secure: true })
    }
    if (path === '/api/v1/me') {
      return signedIn
        ? json(route, user)
        : json(route, { code: 'unauthorized', title: 'Unauthorized', status: 401 }, 401)
    }
    if (path === '/api/v1/stream') {
      // With `buildDelta`, the running build settles; the stream reconnects fast so the delta
      // also lands after the builds list loaded.
      const body = buildDelta
        ? `retry: 300\n${sse([
            [
              'delta',
              {
                kind: 'build',
                seq: 43,
                org: 'o1',
                app: 'kb-shop-prod/web',
                project: 'shop',
                environment: 'prod',
                name: 'web',
                build: { ...builds[0], phase: 'succeeded', finishedAt: now, deployDecision: 'deployed' },
              },
            ],
          ])}`
        : ': ok\n\n'
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body })
    }
    if (path === appPath)
      return json(route, {
        app: currentSource ? { ...app, git_repo: gitRepo } : app,
        pods: [pod('web-web-7d9c-x2x9q', 'web'), pod('web-worker-5f6b-q8w2e', 'worker')],
      })
    if (path === `${appPath}/deployments`) {
      return json(route, approvalMode ? [awaitingRun, ...deployments] : deployments)
    }
    if (approvalMode && path === runPath) return json(route, deploymentRun)
    if (approvalMode && path === `${runPath}/approval`) return json(route, shownApproval)
    if (approvalMode && path === `${runPath}/approve`) {
      const body = route.request().postDataJSON() as { planHash?: string; comment?: string | null }
      if (body.planHash !== planHash) {
        return json(
          route,
          { code: 'conflict', title: 'Conflict', status: 409, detail: 'the plan changed; review it again' },
          409,
        )
      }
      return json(route, {
        ...shownApproval,
        approved: 2,
        canDecide: false,
        decisions: [
          ...approval.decisions,
          { approver: user.email, decision: 'approved', comment: body.comment ?? null, decidedAt: now },
        ],
      })
    }
    if (approvalMode && path === `${runPath}/reject`) {
      return json(
        route,
        { code: 'conflict', title: 'Conflict', status: 409, detail: 'the deployment was already decided' },
        409,
      )
    }
    if (currentSource && path === `${appPath}/builds` && route.request().method() === 'POST') {
      return json(route, { syncOperation: '0190f3c6-0000-7000-8000-0000000000o9', build: builds[0] }, 202)
    }
    const logMatch = currentSource ? new RegExp(`^${appPath}/builds/([^/]+)/logs$`).exec(path) : null
    if (logMatch && url.searchParams.get('follow') === 'true') {
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse(
          buildLogLines.map((line, i): [string, unknown] => [
            'line',
            { pod: 'build', time: iso(now + i * 1000), line },
          ]),
        ),
      })
    }
    if (logMatch) return route.fulfill({ status: 200, contentType: 'text/plain', body: buildLog })
    if (path === `${appPath}/source`) {
      if (route.request().method() === 'PUT') {
        const body = route.request().postDataJSON() as Record<string, unknown>
        currentSource = { ...appSource, ...body, installationId: body.installationId ?? 0, head: null }
        return json(route, currentSource)
      }
      if (currentSource) return json(route, currentSource)
    }
    if (path === '/api/v1/git/connections' && route.request().method() === 'POST') {
      const body = route.request().postDataJSON() as { name: string; provider: string; baseUrl?: string }
      return json(
        route,
        {
          ...gitConnections[0],
          ...body,
          id: '0190f3c6-0000-7000-8000-0000000000g3',
          webhookUrl: '/api/v1/webhooks/gitlab/0190f3c6-0000-7000-8000-0000000000g3',
          webhookSecret: webhookSecret,
        },
        201,
      )
    }
    if (path === '/api/v1/git/connections') return json(route, gitConnections)
    if (path === '/api/v1/git/connections/test') return json(route, connectionCheck)
    const connectionMatch = /^\/api\/v1\/git\/connections\/([^/]+)(?:\/(\w+)(?:-secret)?)?$/.exec(path)
    if (connectionMatch) {
      const [, id, sub] = connectionMatch
      const found = gitConnections.find((c) => c.id === id)
      if (!found) return json(route, { code: 'not_found', title: 'Not Found', status: 404 }, 404)
      if (sub === 'repositories') {
        return json(route, repositoryPages[url.searchParams.get('page') === '2' ? 1 : 0])
      }
      if (sub === 'branches') return json(route, gitBranches)
      if (sub === 'test') return json(route, { ...connectionCheck, username: found.username })
      if (sub === 'webhook') {
        return json(route, { webhookSecret: rotatedWebhookSecret, webhookUrl: found.webhookUrl })
      }
      if (route.request().method() === 'DELETE') {
        return json(
          route,
          { code: 'conflict', title: 'Conflict', status: 409, detail: 'app sources read through it' },
          409,
        )
      }
      if (route.request().method() === 'PATCH') {
        return json(route, { ...found, ...(route.request().postDataJSON() as object) })
      }
      return json(route, found)
    }
    if (path === '/api/v1/git/installations') return json(route, installations)
    if (path === '/api/v1/registries/presets') return json(route, registryPresets)
    if (path === '/api/v1/registries/test') return json(route, { ok: true, error: null, checkedAt: now })
    if (path === '/api/v1/registries' && route.request().method() === 'POST') {
      return json(route, { ...orgRegistries[0], ...(route.request().postDataJSON() as object) }, 201)
    }
    if (path === '/api/v1/registries') return json(route, orgRegistries)
    const registryMatch = /^\/api\/v1\/registries\/([^/]+)(\/test)?$/.exec(path)
    if (registryMatch) {
      const found = orgRegistries.find((r) => r.id === registryMatch[1])
      if (!found) return json(route, { code: 'not_found', title: 'Not Found', status: 404 }, 404)
      if (registryMatch[2]) {
        return json(route, { ok: false, error: 'unauthorized: the token expired', checkedAt: now })
      }
      if (route.request().method() === 'DELETE') return route.fulfill({ status: 204 })
      if (route.request().method() === 'PUT') {
        return json(route, { ...found, updatedAt: now, lastError: null })
      }
      return json(route, found)
    }
    if (gitSource && path === `${appPath}/builds`) return json(route, builds)
    if (gitSource && path === `${appPath}/scans`) return json(route, scans)
    const buildMatch = gitSource ? new RegExp(`^${appPath}/builds/([^/]+)(/cancel)?$`).exec(path) : null
    const found = buildMatch && builds.find((b) => b.id === buildMatch[1])
    if (buildMatch && found) {
      return buildMatch[2]
        ? json(route, { ...found, cancelRequested: true, phase: 'cancelRequested' }, 202)
        : json(route, found)
    }
    if (path === `${appPath}/doctor`) return json(route, doctor)
    if (path === `${appPath}/metrics`) return json(route, metrics)
    if (path === '/api/v1/dns-providers') return json(route, [])
    if (path === `${appPath}/releases`) {
      return json(route, [
        {
          revision: 3,
          image: app.image,
          reason: 'deploy',
          note: null,
          actor: user.email,
          created_at: now,
          current: true,
        },
      ])
    }
    if (path === `${appPath}/logs` && url.searchParams.get('follow') === 'true') {
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([
          [
            'line',
            { pod: 'web-web-7d9c-x2x9q', process: 'web', time: '2026-09-16T10:00:01Z', line: 'GET / 200' },
          ],
          [
            'line',
            {
              pod: 'web-worker-5f6b-q8w2e',
              process: 'worker',
              time: '2026-09-16T10:00:02Z',
              line: 'job done',
            },
          ],
          ['end', { pod: null, error: 'the stream reached its limit; reconnect to go on' }],
        ]),
      })
    }
    if (path === `${appPath}/logs`) {
      return json(route, [
        {
          pod: 'web-web-7d9c-x2x9q',
          process: 'web',
          lines: ['2026-09-16T09:59:00Z previous crash: out of memory'],
          error: null,
        },
      ])
    }
    if (path === '/api/v1/projects') return json(route, projects)
    if (path === '/api/v1/projects/shop') return json(route, projects[0])
    if (path === '/api/v1/projects/shop/environments') return json(route, [environment])
    if (path === '/api/v1/projects/shop/environments/prod') return json(route, environment)
    if (path === '/api/v1/projects/shop/environments/prod/apps') return json(route, [app])
    if (path === '/api/v1/tokens') return json(route, tokens)
    if (path === '/api/v1/members') return json(route, members)
    if (path === '/api/v1/incidents') return json(route, incidents)
    if (path === '/api/v1/webhooks') return json(route, webhooks)
    if (path === `/api/v1/webhooks/${webhooks[0]?.id}/deliveries`) return json(route, deliveries)
    if (path === '/api/v1/domains') return json(route, claims)
    if (path === '/api/v1/audit') return json(route, audit)
    if (path === '/api/v1/healthz/details') return json(route, health)
    if (path === '/api/v1/ci/trust-policies') return json(route, ciPolicies)
    if (path === '/api/v1/auth/sso') {
      return json(route, { enabled: true, displayName: 'Acme SSO', startUrl: '/api/v1/auth/sso/start' })
    }
    if (path === '/api/v1/projects/shop/owner') return json(route, owner)
    if (path === '/api/v1/projects/shop/applications/web/owner') return json(route, null)
    if (path === '/api/v1/projects/shop/environments/prod/freezes') return json(route, freezes)
    if (path === '/api/v1/projects/shop/environments/prod/silences') return json(route, [])
    if (path === '/api/v1/projects/shop/previews') return json(route, previews)
    if (path === '/api/v1/projects/shop/previews/policy') return json(route, previewPolicy)
    if (path === '/api/v1/projects/shop/status-page') return json(route, statusPage)
    if (path === '/api/v1/public/status/shop') return json(route, publicStatus)
    if (path === '/api/v1/projects/shop/environments/prod/detached') return json(route, detached)
    if (
      path === '/api/v1/templates' ||
      path === '/api/v1/projects/shop/environments/prod/secrets' ||
      path === '/api/v1/projects/shop/environments/prod/registries'
    ) {
      return json(route, [])
    }
    return json(
      route,
      { code: 'not_found', title: 'Not Found', status: 404, detail: `no mock for ${path}` },
      404,
    )
  })
}
