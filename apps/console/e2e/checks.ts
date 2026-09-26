import AxeBuilder from '@axe-core/playwright'
import { expect, type Page } from '@playwright/test'

// Checks shared by the mocked suite (console.pw.ts) and the one against a
// running Kuben (live/): preferences, the content security policy, axe.

/** Preferences as the console keeps them, set before it loads. */
export async function prefer(page: Page, locale: string, theme: string) {
  await page.addInitScript(
    (prefs) => window.localStorage.setItem('kuben.prefs', prefs),
    JSON.stringify({ locale, theme }),
  )
}

/** Every violation of the content security policy the page reports. */
export async function watchCsp(page: Page): Promise<string[]> {
  const violations: string[] = []
  await page.exposeFunction('reportCspViolation', (v: string) => violations.push(v))
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      ;(window as unknown as { reportCspViolation: (v: string) => void }).reportCspViolation(
        `${e.violatedDirective} ${e.blockedURI}`,
      )
    })
  })
  return violations
}

export async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .analyze()
  expect(
    results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`),
  ).toEqual([])
}
