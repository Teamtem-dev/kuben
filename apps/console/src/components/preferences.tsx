import { useId } from 'react'
import { LOCALE_NAMES, LOCALES, type Locale, THEMES, type Theme, usePrefs } from '../lib/prefs'

const select =
  'rounded-md border border-line bg-canvas px-2 py-1 text-fg text-xs outline-none focus:border-accent focus:ring-2 focus:ring-accent/30'

/** Language and theme pickers, for the top bar and the sign-in pages. */
export function Preferences() {
  const { locale, theme, setLocale, setTheme, t } = usePrefs()
  const languageId = useId()
  const themeId = useId()
  return (
    <div className="flex items-center gap-2">
      <label htmlFor={languageId} className="sr-only">
        {t('prefs.language')}
      </label>
      <select
        id={languageId}
        value={locale}
        onChange={(e) => setLocale(e.target.value as Locale)}
        className={select}
      >
        {LOCALES.map((l) => (
          <option key={l} value={l} lang={l}>
            {LOCALE_NAMES[l]}
          </option>
        ))}
      </select>
      <label htmlFor={themeId} className="sr-only">
        {t('prefs.theme')}
      </label>
      <select
        id={themeId}
        value={theme}
        onChange={(e) => setTheme(e.target.value as Theme)}
        className={select}
      >
        {THEMES.map((th) => (
          <option key={th} value={th}>
            {t(`theme.${th}`)}
          </option>
        ))}
      </select>
    </div>
  )
}
