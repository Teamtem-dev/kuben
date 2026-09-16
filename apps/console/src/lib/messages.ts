/**
 * Interface text in English and Persian. Every Persian entry is required:
 * `fa` has the type of `en`, so a missing translation does not compile.
 * Pages move their strings here as they are translated.
 */
export const en = {
  'app.name': 'Kuben',
  'common.loading': 'Loading…',
  'common.backToProjects': 'Back to projects',
  'nav.label': 'Main',
  'nav.projects': 'Projects',
  'nav.team': 'Team',
  'nav.tokens': 'API tokens',
  'nav.audit': 'Audit',
  'shell.skip': 'Skip to content',
  'shell.menu': 'Menu',
  'shell.closeMenu': 'Close menu',
  'shell.signOut': 'Sign out',
  'shell.account': 'Account',
  'login.title': 'Sign in to Kuben',
  'login.lead': 'Enter your email and password.',
  'login.email': 'Email',
  'login.password': 'Password',
  'login.submit': 'Sign in',
  'login.submitting': 'Signing in…',
  'prefs.language': 'Language',
  'prefs.theme': 'Theme',
  'theme.system': 'System',
  'theme.light': 'Light',
  'theme.dark': 'Dark',
} as const

export type MessageKey = keyof typeof en

export const fa: Record<MessageKey, string> = {
  'app.name': 'کوبن',
  'common.loading': 'در حال بارگذاری…',
  'common.backToProjects': 'بازگشت به پروژه‌ها',
  'nav.label': 'اصلی',
  'nav.projects': 'پروژه‌ها',
  'nav.team': 'تیم',
  'nav.tokens': 'توکن‌های API',
  'nav.audit': 'رویدادنگاری',
  'shell.skip': 'رفتن به محتوا',
  'shell.menu': 'منو',
  'shell.closeMenu': 'بستن منو',
  'shell.signOut': 'خروج',
  'shell.account': 'حساب کاربری',
  'login.title': 'ورود به کوبن',
  'login.lead': 'ایمیل و رمز عبور خود را وارد کنید.',
  'login.email': 'ایمیل',
  'login.password': 'رمز عبور',
  'login.submit': 'ورود',
  'login.submitting': 'در حال ورود…',
  'prefs.language': 'زبان',
  'prefs.theme': 'تم',
  'theme.system': 'سیستم',
  'theme.light': 'روشن',
  'theme.dark': 'تیره',
}

export const locales = { en, fa } as const
