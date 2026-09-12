/**
 * The Worker behind kuben.teamtem.com. Static assets are served by Workers
 * static assets without running this code; only /api/* and /install.sh reach
 * it (`assets.run_worker_first` in wrangler.jsonc).
 *
 * GET /install.sh is the documented install command,
 * `curl -fsSL https://kuben.teamtem.com/install.sh | sh`: a redirect to the
 * script on the main branch of the repository, so it is always the current
 * one and the site never carries a copy that could fall behind. curl's -L
 * follows it; the script itself downloads only from GitHub releases.
 *
 * POST /api/contact receives the enterprise form (JSON from the page's script,
 * or a plain form post without JavaScript). It drops bots quietly (a honeypot
 * field, a minimum fill time, a foreign Origin), limits each client to five
 * messages an hour (keyed by a hash of the IP, never the IP itself), stores
 * the lead in Workers KV for 180 days, and, when the secret LEADS_WEBHOOK_URL
 * is set, posts a short notification to it (Slack- and Discord-compatible).
 */

interface KVNamespace {
  get(key: string): Promise<string | null>
  put(key: string, value: string, options?: { expirationTtl?: number; metadata?: unknown }): Promise<void>
}

interface ExecutionContext {
  waitUntil(promise: Promise<unknown>): void
}

interface Env {
  LEADS: KVNamespace
  LEADS_WEBHOOK_URL?: string
}

type Fields = Record<string, unknown>

const TOPICS: Record<string, string> = {
  rollout: 'Production rollout',
  security: 'Security and compliance review',
  custom: 'Custom features and integrations',
  training: 'Training and onboarding',
  partnership: 'Partnership',
  other: 'Something else',
}

const LEAD_TTL_SECS = 180 * 24 * 60 * 60
const RATE_LIMIT = 5
const RATE_WINDOW_SECS = 60 * 60
const MIN_FILL_MS = 3000
const MAX_BODY_BYTES = 16 * 1024
const EMAIL = /^[^\s@]{1,64}@[^\s@]{1,190}\.[^\s@]{2,}$/
export const INSTALLER = 'https://raw.githubusercontent.com/Teamtem-dev/kuben/main/install.sh'

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url)
    if (url.pathname === '/install.sh' || url.pathname === '/install') {
      if (request.method !== 'GET' && request.method !== 'HEAD')
        return json({ ok: false, error: 'method_not_allowed' }, 405, { allow: 'GET, HEAD' })
      return new Response(null, {
        status: 302,
        headers: { location: INSTALLER, 'cache-control': 'public, max-age=300' },
      })
    }
    if (url.pathname === '/api/contact' || url.pathname === '/api/contact/') {
      if (request.method !== 'POST')
        return json({ ok: false, error: 'method_not_allowed' }, 405, { allow: 'POST' })
      return contact(request, env, ctx, url)
    }
    return json({ ok: false, error: 'not_found' }, 404)
  },
}

async function contact(request: Request, env: Env, ctx: ExecutionContext, url: URL): Promise<Response> {
  const origin = request.headers.get('origin')
  if (origin && origin !== url.origin) return json({ ok: false, error: 'forbidden' }, 403)

  const type = request.headers.get('content-type') ?? ''
  const isJson = type.includes('application/json')
  if (!isJson && !type.includes('application/x-www-form-urlencoded')) {
    return json({ ok: false, error: 'unsupported_media_type' }, 415)
  }
  const declared = Number(request.headers.get('content-length') ?? 0)
  if (declared > MAX_BODY_BYTES) return reply(isJson, 413, 'too_large')
  const raw = await request.text()
  if (raw.length > MAX_BODY_BYTES) return reply(isJson, 413, 'too_large')

  let data: Fields
  try {
    data = isJson ? (JSON.parse(raw) as Fields) : Object.fromEntries(new URLSearchParams(raw))
  } catch {
    return reply(isJson, 400, 'bad_request')
  }
  if (typeof data !== 'object' || data === null) return reply(isJson, 400, 'bad_request')

  const field = (key: string, max: number) =>
    String(data[key] ?? '')
      .trim()
      .slice(0, max)

  // Bots: answer as if it worked, store nothing.
  if (field('website', 200)) return accepted(isJson)
  const startedAt = Number(data.t)
  if (Number.isFinite(startedAt) && startedAt > 0 && Date.now() - startedAt < MIN_FILL_MS)
    return accepted(isJson)

  const lead = {
    name: field('name', 100),
    company: field('company', 120),
    email: field('email', 200).toLowerCase(),
    topic: field('topic', 40),
    message: field('message', 5000),
  }
  const invalid = [
    lead.name.length < 1 && 'name',
    !EMAIL.test(lead.email) && 'email',
    !(lead.topic in TOPICS) && 'topic',
    lead.message.length < 10 && 'message',
  ].filter(Boolean)
  if (invalid.length > 0) {
    return isJson
      ? json({ ok: false, error: 'invalid', fields: invalid }, 422)
      : redirect(url, '/enterprise/?error=invalid')
  }

  const client = request.headers.get('cf-connecting-ip') ?? 'unknown'
  const bucket = Math.floor(Date.now() / 1000 / RATE_WINDOW_SECS)
  const rateKey = `rl:${await sha256(`${client}:${bucket}`)}`
  const count = Number((await env.LEADS.get(rateKey)) ?? 0)
  if (count >= RATE_LIMIT) return reply(isJson, 429, 'rate_limited')
  await env.LEADS.put(rateKey, String(count + 1), { expirationTtl: RATE_WINDOW_SECS + 60 })

  const receivedAt = new Date().toISOString()
  const country = (request as Request & { cf?: { country?: string } }).cf?.country ?? null
  const record = {
    ...lead,
    topicLabel: TOPICS[lead.topic],
    receivedAt,
    country,
    userAgent: (request.headers.get('user-agent') ?? '').slice(0, 300),
  }
  await env.LEADS.put(`lead:${receivedAt}:${crypto.randomUUID()}`, JSON.stringify(record), {
    expirationTtl: LEAD_TTL_SECS,
    metadata: { email: lead.email, company: lead.company, topic: lead.topic },
  })

  if (env.LEADS_WEBHOOK_URL) {
    const text = [
      `New Kuben enterprise inquiry: ${record.topicLabel}`,
      `${lead.name}${lead.company ? ` (${lead.company})` : ''} <${lead.email}>`,
      '',
      lead.message.slice(0, 1500),
    ].join('\n')
    ctx.waitUntil(
      fetch(env.LEADS_WEBHOOK_URL, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ text, content: text }),
      }).catch(() => undefined),
    )
  }

  return accepted(isJson)
}

function accepted(isJson: boolean): Response {
  return isJson
    ? json({ ok: true }, 200)
    : new Response(null, { status: 303, headers: { location: '/enterprise/thanks/' } })
}

function reply(isJson: boolean, status: number, error: string): Response {
  return isJson
    ? json({ ok: false, error }, status)
    : new Response(null, { status: 303, headers: { location: `/enterprise/?error=${error}` } })
}

function redirect(url: URL, path: string): Response {
  return new Response(null, {
    status: 303,
    headers: { location: new URL(path, url).pathname + new URL(path, url).search },
  })
}

function json(body: unknown, status: number, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'content-type': 'application/json; charset=utf-8',
      'cache-control': 'no-store',
      'x-content-type-options': 'nosniff',
      ...headers,
    },
  })
}

async function sha256(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('')
}
