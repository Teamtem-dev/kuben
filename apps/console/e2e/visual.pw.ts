// Visual snapshots of the key pages, in English and Persian (LTR and RTL),
// light and dark, against the mocked API with the clock pinned.
//
// Snapshots live in __screenshots__/ (see playwright.config.ts). A missing
// snapshot is written, not compared, so a new page or platform never fails
// its first run; CI uploads the folder as the `console-screenshots`
// artifact. To refresh after an intended change (on Linux, like CI):
//   cd apps/console && bun run build
//   cd e2e && npx playwright test visual.pw.ts --update-snapshots
// then commit __screenshots__/.
import { existsSync } from 'node:fs'
import { expect, type Page, test } from '@playwright/test'
import { mockApi, now, prefer } from './fixtures'

const pages = [
  { name: 'home', path: '/', ready: (p: Page) => p.getByRole('heading', { level: 2 }).first() },
  { name: 'app', path: '/projects/shop/prod/web', ready: (p: Page) => p.getByText('web-web-7d9c-x2x9q') },
  {
    name: 'environment',
    path: '/projects/shop/prod',
    ready: (p: Page) => p.getByText('end-of-quarter close'),
  },
  { name: 'incidents', path: '/incidents', ready: (p: Page) => p.getByRole('table') },
  { name: 'audit', path: '/audit', ready: (p: Page) => p.getByRole('table') },
  { name: 'settings', path: '/settings', ready: (p: Page) => p.getByRole('table') },
  {
    name: 'login',
    path: '/login',
    ready: (p: Page) => p.getByRole('heading', { level: 1 }),
    signedIn: false,
  },
] as const

for (const locale of ['en', 'fa'] as const) {
  for (const theme of ['light', 'dark'] as const) {
    for (const view of pages) {
      test(`visual: ${view.name} (${locale}, ${theme})`, async ({ page }, testInfo) => {
        await page.clock.setFixedTime(now)
        await page.setViewportSize({ width: 1280, height: 800 })
        await prefer(page, locale, theme)
        await mockApi(page, { signedIn: !('signedIn' in view) })
        await page.goto(view.path)
        await expect(view.ready(page)).toBeVisible()
        // Nothing still loading: no skeleton, no pending status.
        await expect(page.locator('[data-slot="skeleton"]')).toHaveCount(0)
        await page.evaluate(() => document.fonts.ready)

        const name = `${view.name}-${locale}-${theme}.png`
        const file = testInfo.snapshotPath(name, { kind: 'screenshot' })
        if (!existsSync(file)) {
          await page.screenshot({
            path: file,
            fullPage: true,
            animations: 'disabled',
            caret: 'hide',
            scale: 'css',
          })
          testInfo.annotations.push({ type: 'snapshot', description: `written ${name}` })
          return
        }
        await expect(page).toHaveScreenshot(name, { fullPage: true })
      })
    }
  }
}
