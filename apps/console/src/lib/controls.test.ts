import { expect, test } from 'bun:test'
import { lines, localInput, rfc3339, windowState } from './controls'

const now = Date.UTC(2026, 8, 16, 10, 0, 0)
const iso = (ms: number) => new Date(ms).toISOString()

test('a window is lifted, active, scheduled or ended', () => {
  const w = { active: false, liftedAt: null, startsAt: iso(now - 1000), endsAt: iso(now + 1000) }
  expect(windowState({ ...w, active: true }, now)).toBe('active')
  expect(windowState({ ...w, liftedAt: iso(now) }, now)).toBe('lifted')
  expect(windowState({ ...w, startsAt: iso(now + 500) }, now)).toBe('scheduled')
  expect(windowState({ ...w, endsAt: iso(now - 500) }, now)).toBe('ended')
})

test('datetime-local values round-trip through RFC 3339', () => {
  const value = localInput(now)
  expect(value).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/)
  expect(rfc3339(value)).toBe(iso(now))
  expect(rfc3339('')).toBeNull()
  expect(rfc3339('not a date')).toBeNull()
})

test('lines splits on newlines and commas, dropping blanks', () => {
  expect(lines(' refs/heads/main \n\nrefs/tags/v*, production ')).toEqual([
    'refs/heads/main',
    'refs/tags/v*',
    'production',
  ])
})
