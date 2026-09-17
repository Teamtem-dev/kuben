import { describe, expect, test } from 'bun:test'
import { fill, pagesEn, pagesFa } from './pages'

describe('fill', () => {
  test('replaces every placeholder with its value', () => {
    expect(fill('Type "{name}" to delete this {what}', { name: 'shop', what: 'project' })).toBe(
      'Type "shop" to delete this project',
    )
    expect(fill('{count} projects, {count} again', { count: 3 })).toBe('3 projects, 3 again')
  })

  test('leaves unknown placeholders and plain text alone', () => {
    expect(fill('Pods ({count})', {})).toBe('Pods ({count})')
    expect(fill('No placeholders', { x: 1 })).toBe('No placeholders')
    expect(fill('{toString}', {})).toBe('{toString}')
  })

  test('Persian messages keep the placeholders of the English ones', () => {
    const names = (s: string) => (s.match(/\{\w+\}/g) ?? []).sort()
    for (const key of Object.keys(pagesEn) as (keyof typeof pagesEn)[]) {
      expect([key, names(pagesFa[key])]).toEqual([key, names(pagesEn[key])])
    }
  })
})
