/**
 * The viewer's language and theme: kept in this browser, applied to <html>
 * (`lang`, `dir`, `data-theme`) before the first paint and whenever they
 * change. `system` follows the operating system's light or dark setting.
 */
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { locales, type MessageKey } from './messages'

export type Locale = keyof typeof locales
export type Theme = 'system' | 'light' | 'dark'

export interface Prefs {
  locale: Locale
  theme: Theme
}

const KEY = 'kuben.prefs'
const LOCALES = Object.keys(locales) as Locale[]
const THEMES: Theme[] = ['system', 'light', 'dark']

/** Right-to-left scripts among the locales. */
export const direction = (locale: Locale): 'rtl' | 'ltr' => (locale === 'fa' ? 'rtl' : 'ltr')

/** Stored preferences, or what the browser asks for. Anything unreadable is ignored. */
export function parsePrefs(stored: string | null, languages: readonly string[]): Prefs {
  let value: Partial<Prefs> = {}
  try {
    value = stored ? (JSON.parse(stored) as Partial<Prefs>) : {}
  } catch {
    value = {}
  }
  const asked = languages
    .map((l) => l.slice(0, 2).toLowerCase())
    .find((l): l is Locale => LOCALES.includes(l as Locale))
  return {
    locale: LOCALES.includes(value.locale as Locale) ? (value.locale as Locale) : (asked ?? 'en'),
    theme: THEMES.includes(value.theme as Theme) ? (value.theme as Theme) : 'system',
  }
}

function storage(): Storage | undefined {
  try {
    return window.localStorage
  } catch {
    return undefined
  }
}

export function readPrefs(): Prefs {
  let stored: string | null = null
  try {
    stored = storage()?.getItem(KEY) ?? null
  } catch {
    stored = null
  }
  return parsePrefs(stored, navigator.languages ?? [navigator.language])
}

const darkQuery = () => window.matchMedia('(prefers-color-scheme: dark)')

/** Light or dark, as `theme` resolves right now. */
export const resolveTheme = (theme: Theme, systemDark: boolean): 'light' | 'dark' =>
  theme === 'system' ? (systemDark ? 'dark' : 'light') : theme

export function applyPrefs({ locale, theme }: Prefs) {
  const root = document.documentElement
  root.lang = locale
  root.dir = direction(locale)
  root.dataset.theme = resolveTheme(theme, darkQuery().matches)
}

interface PrefsValue extends Prefs {
  setLocale: (locale: Locale) => void
  setTheme: (theme: Theme) => void
  t: (key: MessageKey) => string
  /** A message whose key is made at run time (a phase, a check), else `fallback`. */
  tOr: (key: string, fallback: string) => string
}

const PrefsContext = createContext<PrefsValue | null>(null)

export function PrefsProvider({ children }: { children: ReactNode }) {
  const [prefs, setPrefs] = useState(readPrefs)

  useEffect(() => {
    applyPrefs(prefs)
    try {
      storage()?.setItem(KEY, JSON.stringify(prefs))
    } catch {
      // Private windows may refuse; the choice then lasts for this page.
    }
    if (prefs.theme !== 'system') return
    const query = darkQuery()
    const follow = () => applyPrefs(prefs)
    query.addEventListener('change', follow)
    return () => query.removeEventListener('change', follow)
  }, [prefs])

  const setLocale = useCallback((locale: Locale) => setPrefs((p) => ({ ...p, locale })), [])
  const setTheme = useCallback((theme: Theme) => setPrefs((p) => ({ ...p, theme })), [])
  const value = useMemo<PrefsValue>(
    () => ({
      ...prefs,
      setLocale,
      setTheme,
      t: (key) => locales[prefs.locale][key],
      tOr: (key, fallback) => (locales[prefs.locale] as Record<string, string>)[key] ?? fallback,
    }),
    [prefs, setLocale, setTheme],
  )
  return <PrefsContext.Provider value={value}>{children}</PrefsContext.Provider>
}

export function usePrefs(): PrefsValue {
  const value = useContext(PrefsContext)
  if (!value) throw new Error('usePrefs outside PrefsProvider')
  return value
}

export const LOCALE_NAMES: Record<Locale, string> = { en: 'English', fa: 'فارسی' }
export { LOCALES, THEMES }
