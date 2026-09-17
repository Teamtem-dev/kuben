import { describe, expect, test } from 'bun:test'
import { bytes, cpu, duration, incidentState, sparkline, when } from './ops'

describe('operational formatting', () => {
  test('durations use their two largest units', () => {
    expect(duration(2 * 3600 + 5 * 60 + 9, 'en')).toBe('2 hr 5 min')
    expect(duration(3 * 86_400 + 3600 + 60, 'en')).toBe('3 days 1 hr')
    expect(duration(0, 'en')).toBe('0 min')
    expect(duration(-5, 'en')).toBe('0 min')
    expect(duration(3600, 'fa')).not.toBe(duration(3600, 'en'))
  })

  test('sizes and cpu read naturally', () => {
    expect(bytes(512, 'en')).toBe('512 B')
    expect(bytes(64 * 1024 * 1024, 'en')).toBe('64 MiB')
    expect(bytes(1.5 * 1024 ** 3, 'en')).toBe('1.5 GiB')
    expect(cpu(250, 'en', 'cores')).toBe('250m')
    expect(cpu(1500, 'en', 'cores')).toBe('1.5 cores')
  })

  test('dates fall back to their text', () => {
    expect(when('not a date', 'en')).toBe('not a date')
    expect(when(null, 'en')).toBe('')
    expect(when('2026-09-17T10:00:00Z', 'en')).toContain('2026')
  })

  test('sparklines fit their box', () => {
    expect(sparkline([], 100, 20)).toBe('')
    expect(sparkline([5], 100, 20)).toBe('50,0')
    expect(sparkline([0, 10], 100, 20)).toBe('0,20 100,0')
    expect(sparkline([0, 0], 100, 20)).toBe('0,20 100,20')
  })

  test('incidents are open, acknowledged or resolved', () => {
    expect(incidentState({})).toBe('open')
    expect(incidentState({ acknowledgedAt: 'x' })).toBe('acknowledged')
    expect(incidentState({ acknowledgedAt: 'x', resolvedAt: 'y' })).toBe('resolved')
  })
})
