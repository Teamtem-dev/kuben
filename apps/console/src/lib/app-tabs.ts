/** The app page's tabs, in order; the first is the default and stays out of the URL. */
export const APP_TABS = ['overview', 'deployments', 'builds', 'releases', 'logs', 'settings'] as const

export type AppTab = (typeof APP_TABS)[number]

/** `?tab=` as the app page reads it: a known tab other than the default, else nothing. */
export function appTabFrom(value: unknown): Exclude<AppTab, 'overview'> | undefined {
  return typeof value === 'string' && value !== 'overview' && (APP_TABS as readonly string[]).includes(value)
    ? (value as Exclude<AppTab, 'overview'>)
    : undefined
}

/** The tabs an app shows: builds only when it is built from a Git source. */
export function appTabs(gitSource: boolean): readonly AppTab[] {
  return gitSource ? APP_TABS : APP_TABS.filter((tab) => tab !== 'builds')
}

/** The tab to show for `?tab=`: one the app does not have falls back to the overview. */
export function shownAppTab(tab: AppTab | undefined, gitSource: boolean): AppTab {
  return tab && appTabs(gitSource).includes(tab) ? tab : 'overview'
}

/** `?build=` as the builds tab reads it: a build id, else nothing. */
export function buildFrom(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' ? value : undefined
}
