import { describe, expect, test } from 'bun:test'
import { appendLines, endFrom, lineFrom, splitTimestamp } from './logStream'

describe('followed logs', () => {
  test('lines are kept in order and bounded', () => {
    const make = (n: number) => Array.from({ length: n }, (_, i) => ({ id: i, pod: 'p', text: `l${i}` }))
    const kept = appendLines(
      make(3),
      make(4).map((l) => ({ ...l, id: l.id + 3 })),
      5,
    )
    expect(kept.map((l) => l.id)).toEqual([2, 3, 4, 5, 6])
  })

  test('events become lines and notes', () => {
    expect(lineFrom('{"pod":"web-1","time":"t","line":"hello"}', 1)).toEqual({
      id: 1,
      pod: 'web-1',
      time: 't',
      text: 'hello',
    })
    expect(lineFrom('nonsense', 2)).toBeUndefined()
    expect(endFrom('{"pod":"web-1","error":"container restarted"}', 3)).toEqual({
      final: false,
      note: { id: 3, pod: 'web-1', text: 'container restarted', note: true },
    })
    expect(endFrom('{"pod":null,"error":null}', 4)?.final).toBe(true)
  })

  test('timestamps are split off', () => {
    expect(splitTimestamp('2026-09-16T10:00:00.1Z GET / 200')).toEqual({
      time: '2026-09-16T10:00:00.1Z',
      text: 'GET / 200',
    })
    expect(splitTimestamp('plain line')).toEqual({ text: 'plain line' })
  })
})
