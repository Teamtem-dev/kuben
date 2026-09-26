/** The app page's tabs, in order; the first is the default and stays out of the URL. */
export const APP_TABS = ['overview', 'deployments', 'releases', 'logs', 'settings'] as const

export type AppTab = (typeof APP_TABS)[number]

/** `?tab=` as the app page reads it: a known tab other than the default, else nothing. */
export function appTabFrom(value: unknown): Exclude<AppTab, 'overview'> | undefined {
  return typeof value === 'string' && value !== 'overview' && (APP_TABS as readonly string[]).includes(value)
    ? (value as Exclude<AppTab, 'overview'>)
    : undefined
}
