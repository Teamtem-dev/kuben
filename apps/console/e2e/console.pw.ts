import { expect, test } from '@playwright/test'
import { expectAccessible, prefer, watchCsp } from './checks'
import { mockApi } from './fixtures'

const locales = [
  {
    locale: 'en',
    dir: 'ltr',
    signIn: 'Sign in to Kuben',
    deployments: 'Deployments',
    live: 'Live',
    doctor: 'Doctor',
  },
  {
    locale: 'fa',
    dir: 'rtl',
    signIn: 'ورود به کوبن',
    deployments: 'استقرارها',
    live: 'زنده',
    doctor: 'Doctor',
  },
] as const

const themes = ['dark', 'light'] as const

for (const l of locales) {
  for (const theme of themes) {
    test.describe(`${l.locale} ${l.dir} ${theme}`, () => {
      test('sign-in page', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page, { signedIn: false })
        await page.goto('/login')
        await expect(page.getByRole('heading', { name: l.signIn })).toBeVisible()
        await expect(page.locator('html')).toHaveAttribute('dir', l.dir)
        await expect(page.locator('html')).toHaveAttribute('lang', l.locale)
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
        await expectAccessible(page)
        expect(csp).toEqual([])
      })

      test('app page: deployment timeline and live logs', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/projects/shop/prod/web')
        await expect(page.getByRole('heading', { name: l.deployments })).toBeVisible()
        await expect(page.getByText('ghcr.io/acme/web@sha256:0123', { exact: false })).toBeVisible()
        const log = page.locator('pre[dir="ltr"]')
        await expect(log).toContainText('GET / 200')
        await expect(log).toContainText('[web-worker-5f6b-q8w2e]')
        await expect(page.getByRole('status')).toBeVisible()
        await expectAccessible(page)
        expect(csp).toEqual([])
      })

      test('previous container logs', async ({ page }) => {
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/projects/shop/prod/web')
        const previous = l.locale === 'fa' ? 'container قبلی' : 'Previous container'
        await page.getByText(previous, { exact: true }).click()
        await expect(page.locator('pre[dir="ltr"]')).toContainText('previous crash: out of memory')
      })

      test('Doctor page', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/projects/shop/prod/web/doctor')
        await expect(page.getByRole('heading', { name: new RegExp(l.doctor) })).toBeVisible()
        await expect(page.getByText('no DNS record', { exact: true })).toBeVisible()
        await expect(
          page.getByText('point the record at the Gateway', { exact: false }).first(),
        ).toBeVisible()
        await expectAccessible(page)
        expect(csp).toEqual([])
      })
    })
  }
}

test('the menu opens as a drawer on a phone and closes on navigation', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  const nav = page.getByRole('navigation', { name: 'Main' })
  await expect(nav).toBeHidden()
  await page.getByRole('button', { name: 'Menu' }).click()
  await expect(nav).toBeVisible()
  await nav.getByRole('link', { name: 'Team' }).click()
  await expect(nav).toBeHidden()
})
