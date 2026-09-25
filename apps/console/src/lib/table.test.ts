import { describe, expect, test } from 'bun:test'
import { filterRows, nextSort, normalize, paginate, sortRows } from './table'

describe('filterRows', () => {
  const rows = ['app.update shop/prod/web', 'Member removed', 'کاربر حذف شد', 'Café']
  const text = (s: string) => s

  test('every word must appear, in any order and case', () => {
    expect(filterRows(rows, 'WEB shop', text)).toEqual(['app.update shop/prod/web'])
    expect(filterRows(rows, 'removed member', text)).toEqual(['Member removed'])
    expect(filterRows(rows, 'shop nope', text)).toEqual([])
  })

  test('an empty query keeps every row', () => {
    expect(filterRows(rows, '   ', text)).toEqual(rows)
  })

  test('accents and Arabic-keyboard letters fold', () => {
    expect(filterRows(rows, 'cafe', text)).toEqual(['Café'])
    // Arabic kaf and yeh, as a Persian user may type them.
    expect(filterRows(rows, 'كاربر', text)).toEqual(['کاربر حذف شد'])
    expect(normalize('علي')).toBe(normalize('علی'))
  })
})

describe('sortRows', () => {
  const rows = [
    { n: 'b10', v: 3 },
    { n: 'b2', v: null },
    { n: 'a', v: 1 },
    { n: '', v: 2 },
  ]

  test('strings compare numerically, empty values last both ways', () => {
    expect(sortRows(rows, (r) => r.n, false, 'en').map((r) => r.n)).toEqual(['a', 'b2', 'b10', ''])
    expect(sortRows(rows, (r) => r.n, true, 'en').map((r) => r.n)).toEqual(['b10', 'b2', 'a', ''])
  })

  test('numbers compare as numbers; nulls go last', () => {
    expect(sortRows(rows, (r) => r.v, false, 'en').map((r) => r.v)).toEqual([1, 2, 3, null])
    expect(sortRows(rows, (r) => r.v, true, 'en').map((r) => r.v)).toEqual([3, 2, 1, null])
  })

  test('stable for equal values', () => {
    const same = [
      { k: 1, id: 'x' },
      { k: 1, id: 'y' },
      { k: 0, id: 'z' },
    ]
    expect(sortRows(same, (r) => r.k, false, 'en').map((r) => r.id)).toEqual(['z', 'x', 'y'])
    expect(sortRows(same, (r) => r.k, true, 'en').map((r) => r.id)).toEqual(['x', 'y', 'z'])
  })
})

test('nextSort cycles ascending, descending, none; another column starts ascending', () => {
  expect(nextSort(null, 'a')).toEqual({ id: 'a', desc: false })
  expect(nextSort({ id: 'a', desc: false }, 'a')).toEqual({ id: 'a', desc: true })
  expect(nextSort({ id: 'a', desc: true }, 'a')).toBeNull()
  expect(nextSort({ id: 'a', desc: true }, 'b')).toEqual({ id: 'b', desc: false })
})

describe('paginate', () => {
  const rows = Array.from({ length: 23 }, (_, i) => i)

  test('slices pages and reports the range', () => {
    expect(paginate(rows, 0, 10)).toEqual({
      rows: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
      page: 0,
      pages: 3,
      from: 1,
      to: 10,
    })
    expect(paginate(rows, 2, 10)).toMatchObject({ rows: [20, 21, 22], page: 2, from: 21, to: 23 })
  })

  test('a page out of range is clamped; no rows is one empty page', () => {
    expect(paginate(rows, 9, 10).page).toBe(2)
    expect(paginate(rows, -1, 10).page).toBe(0)
    expect(paginate([], 0, 10)).toEqual({ rows: [], page: 0, pages: 1, from: 0, to: 0 })
  })
})
