// Tests for the site Worker (`bun test worker`, part of `turbo run test`).
// The Worker's fetch handler is called directly with an in-memory KV, so no
// workerd or network is needed.
import { beforeEach, describe, expect, test } from 'bun:test'
import worker from './index'

type Entry = { value: string; metadata?: unknown; ttl?: number }

class MemoryKV {
  store = new Map<string, Entry>()
  async get(key: string) {
    return this.store.get(key)?.value ?? null
  }
  async put(key: string, value: string, options?: { expirationTtl?: number; metadata?: unknown }) {
    this.store.set(key, { value, metadata: options?.metadata, ttl: options?.expirationTtl })
  }
  leads() {
    return [...this.store.entries()].filter(([k]) => k.startsWith('lead:'))
  }
}

const SITE = 'https://kuben.teamtem.com'
const IP = '203.0.113.7'
let kv: MemoryKV
let pending: Promise<unknown>[]
const ctx = { waitUntil: (p: Promise<unknown>) => void pending.push(p) }

function call(
  body: string | null,
  opts: { type?: string; origin?: string | null; ip?: string; method?: string; path?: string } = {},
  env: Record<string, unknown> = {},
) {
  const headers: Record<string, string> = { 'cf-connecting-ip': opts.ip ?? IP }
  if (opts.type !== undefined || body !== null) headers['content-type'] = opts.type ?? 'application/json'
  if (opts.origin !== null) headers.origin = opts.origin ?? SITE
  const request = new Request(`${SITE}${opts.path ?? '/api/contact'}`, {
    method: opts.method ?? 'POST',
    headers,
    body: body ?? undefined,
  })
  return worker.fetch(request, { LEADS: kv, ...env } as never, ctx)
}

const lead = (extra: Record<string, unknown> = {}) =>
  JSON.stringify({
    name: 'Ada Tester',
    company: 'Acme',
    email: 'Ada@Acme.example',
    topic: 'rollout',
    message: 'We run 40 services on k3s and want HA.',
    website: '',
    t: String(Date.now() - 10_000),
    ...extra,
  })

beforeEach(() => {
  kv = new MemoryKV()
  pending = []
})

describe('POST /api/contact', () => {
  test('stores a valid lead for 180 days, without the client IP', async () => {
    const res = await call(lead())
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ ok: true })
    const leads = kv.leads()
    expect(leads).toHaveLength(1)
    const [key, entry] = leads[0] as [string, Entry]
    expect(key).toMatch(/^lead:\d{4}-\d{2}-\d{2}T.*:[0-9a-f-]{36}$/)
    expect(entry.ttl).toBe(180 * 24 * 60 * 60)
    expect(entry.metadata).toEqual({ email: 'ada@acme.example', company: 'Acme', topic: 'rollout' })
    const record = JSON.parse(entry.value)
    expect(record).toMatchObject({ name: 'Ada Tester', email: 'ada@acme.example', topicLabel: 'Production rollout' })
    const everything = JSON.stringify([...kv.store.entries()])
    expect(everything).not.toContain(IP)
  })

  test('answers bots as if it worked but stores nothing', async () => {
    for (const body of [lead({ website: 'http://spam.example' }), lead({ t: String(Date.now()) })]) {
      const res = await call(body)
      expect(res.status).toBe(200)
      expect(await res.json()).toEqual({ ok: true })
    }
    expect(kv.store.size).toBe(0)
  })

  test('rejects invalid fields with the list of fields', async () => {
    const res = await call(JSON.stringify({ name: '', email: 'nope', topic: 'x', message: 'short' }))
    expect(res.status).toBe(422)
    expect(await res.json()).toEqual({ ok: false, error: 'invalid', fields: ['name', 'email', 'topic', 'message'] })
    expect(kv.leads()).toHaveLength(0)
  })

  test('refuses other origins, other content types, oversized and malformed bodies', async () => {
    expect((await call(lead(), { origin: 'https://evil.example' })).status).toBe(403)
    expect((await call('hello', { type: 'text/plain' })).status).toBe(415)
    expect((await call(lead({ message: 'x'.repeat(20_000) }))).status).toBe(413)
    expect((await call('{not json')).status).toBe(400)
    expect(kv.store.size).toBe(0)
  })

  test('accepts a plain form post and redirects to the thanks page', async () => {
    const form = new URLSearchParams({
      name: 'Grace',
      email: 'grace@acme.example',
      topic: 'security',
      message: 'Please review our RBAC setup.',
    })
    const res = await call(form.toString(), { type: 'application/x-www-form-urlencoded' })
    expect(res.status).toBe(303)
    expect(res.headers.get('location')).toBe('/enterprise/thanks/')
    expect(kv.leads()).toHaveLength(1)
  })

  test('redirects an invalid form post back to the form with the reason', async () => {
    const res = await call(new URLSearchParams({ name: 'x' }).toString(), { type: 'application/x-www-form-urlencoded' })
    expect(res.status).toBe(303)
    expect(res.headers.get('location')).toBe('/enterprise/?error=invalid')
  })

  test('allows five messages per client per hour', async () => {
    for (let i = 0; i < 5; i++) expect((await call(lead())).status).toBe(200)
    const limited = await call(lead())
    expect(limited.status).toBe(429)
    expect(await limited.json()).toEqual({ ok: false, error: 'rate_limited' })
    expect((await call(lead(), { ip: '198.51.100.9' })).status).toBe(200)
    expect(kv.leads()).toHaveLength(6)
  })

  test('posts a notification when LEADS_WEBHOOK_URL is set', async () => {
    const calls: { url: string; body: string }[] = []
    const original = globalThis.fetch
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      calls.push({ url, body: String(init?.body) })
      return new Response('ok')
    }) as unknown as typeof fetch
    try {
      const res = await call(lead(), {}, { LEADS_WEBHOOK_URL: 'https://hooks.example/abc' })
      expect(res.status).toBe(200)
      await Promise.all(pending)
    } finally {
      globalThis.fetch = original
    }
    expect(calls).toHaveLength(1)
    const payload = JSON.parse(calls[0]?.body ?? '{}')
    expect(calls[0]?.url).toBe('https://hooks.example/abc')
    expect(payload.text).toContain('Production rollout')
    expect(payload.content).toBe(payload.text)
  })
})

describe('other routes', () => {
  test('GET /api/contact is not allowed', async () => {
    const res = await call(null, { method: 'GET' })
    expect(res.status).toBe(405)
    expect(res.headers.get('allow')).toBe('POST')
  })

  test('unknown /api paths are 404 JSON, never cached', async () => {
    const res = await call(null, { method: 'GET', path: '/api/other' })
    expect(res.status).toBe(404)
    expect(res.headers.get('cache-control')).toBe('no-store')
    expect(await res.json()).toEqual({ ok: false, error: 'not_found' })
  })
})
