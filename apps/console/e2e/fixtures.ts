import { existsSync, readFileSync, statSync } from 'node:fs'
import { extname, join, normalize } from 'node:path'
import type { Page, Route } from '@playwright/test'

const dist = join(import.meta.dirname, '..', 'dist')

/** The content security policy Kuben serves the console with, from its source. */
function kubenCsp(): string {
  const file = '../../../internal/httpapi/web/web.go'
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

const now = Date.UTC(2026, 8, 16, 10, 0, 0)

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
    uid: null,
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

const json = (route: Route, body: unknown, status = 200) =>
  route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })

/**
 * Answer the console's API calls; `signedIn: false` shows the sign-in page,
 * `setupNeeded: true` the first-run setup.
 */
export async function mockApi(page: Page, { signedIn = true, setupNeeded = false } = {}) {
  const appPath = '/api/v1/projects/shop/environments/prod/apps/web'
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
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': ok\n\n' })
    }
    if (path === appPath)
      return json(route, {
        app,
        pods: [pod('web-web-7d9c-x2x9q', 'web'), pod('web-worker-5f6b-q8w2e', 'worker')],
      })
    if (path === `${appPath}/deployments`) return json(route, deployments)
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
