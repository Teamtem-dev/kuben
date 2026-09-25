import { LanguagesIcon, type LucideIcon, MonitorIcon, MoonIcon, SunIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { LOCALE_NAMES, LOCALES, type Locale, THEMES, type Theme, usePrefs } from '@/lib/prefs'

const THEME_ICONS: Record<Theme, LucideIcon> = { system: MonitorIcon, light: SunIcon, dark: MoonIcon }

/** The language picker of the top bar and the sign-in pages. */
export function LanguageMenu() {
  const { locale, setLocale, t } = usePrefs()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label={t('prefs.language')} title={t('prefs.language')}>
          <LanguagesIcon aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel>{t('prefs.language')}</DropdownMenuLabel>
        <DropdownMenuRadioGroup value={locale} onValueChange={(v) => setLocale(v as Locale)}>
          {LOCALES.map((l) => (
            <DropdownMenuRadioItem key={l} value={l} lang={l}>
              {LOCALE_NAMES[l]}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** The theme picker of the top bar and the sign-in pages. */
export function ThemeMenu() {
  const { theme, setTheme, t } = usePrefs()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label={t('prefs.theme')} title={t('prefs.theme')}>
          <SunIcon className="dark:hidden" aria-hidden="true" />
          <MoonIcon className="hidden dark:block" aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel>{t('prefs.theme')}</DropdownMenuLabel>
        <DropdownMenuRadioGroup value={theme} onValueChange={(v) => setTheme(v as Theme)}>
          {THEMES.map((th) => {
            const Icon = THEME_ICONS[th]
            return (
              <DropdownMenuRadioItem key={th} value={th}>
                <Icon aria-hidden="true" />
                {t(`theme.${th}`)}
              </DropdownMenuRadioItem>
            )
          })}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
