import { describe, expect, test } from 'bun:test'
import { en, fa } from './messages'
import { direction, parsePrefs, resolveTheme } from './prefs'

describe('preferences', () => {
  test('stored choices win, then the browser language, then English', () => {
    expect(parsePrefs('{"locale":"fa","theme":"light"}', ['en-US'])).toEqual({ locale: 'fa', theme: 'light' })
    expect(parsePrefs(null, ['fa-IR', 'en'])).toEqual({ locale: 'fa', theme: 'system' })
    expect(parsePrefs(null, ['de-DE'])).toEqual({ locale: 'en', theme: 'system' })
  })

  test('anything unreadable is ignored', () => {
    expect(parsePrefs('not json', [])).toEqual({ locale: 'en', theme: 'system' })
    expect(parsePrefs('{"locale":"xx","theme":"neon"}', ['fa'])).toEqual({ locale: 'fa', theme: 'system' })
  })

  test('Persian is right to left and system follows the OS', () => {
    expect(direction('fa')).toBe('rtl')
    expect(direction('en')).toBe('ltr')
    expect(resolveTheme('system', true)).toBe('dark')
    expect(resolveTheme('system', false)).toBe('light')
    expect(resolveTheme('light', true)).toBe('light')
  })

  test('every English message has a Persian one', () => {
    for (const key of Object.keys(en) as (keyof typeof en)[]) {
      expect(fa[key].trim().length).toBeGreaterThan(0)
    }
  })
})
