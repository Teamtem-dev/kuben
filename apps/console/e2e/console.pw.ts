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

test('the sidebar opens as a drawer on a phone and closes on navigation', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  const nav = page.getByRole('navigation', { name: 'Main' })
  await expect(nav).toBeHidden()
  await page.getByRole('banner').getByRole('button', { name: 'Toggle sidebar' }).click()
  await expect(nav).toBeVisible()
  await nav.getByRole('link', { name: 'Team' }).click()
  await expect(page).toHaveURL(/\/team$/)
  await expect(nav).toBeHidden()
})

for (const l of locales) {
  test(`${l.locale}: the sidebar sits on the start side and collapses to icons`, async ({ page }) => {
    await prefer(page, l.locale, 'light')
    await mockApi(page)
    await page.goto('/projects/shop/prod/web')
    const nav = page.getByRole('navigation', { name: l.locale === 'fa' ? 'اصلی' : 'Main' })
    await expect(nav).toBeVisible()
    const box = await nav.boundingBox()
    const width = page.viewportSize()?.width ?? 0
    expect(box).not.toBeNull()
    if (box) expect(l.dir === 'rtl' ? box.x > width / 2 : box.x < width / 2).toBe(true)
    const toggle = page
      .getByRole('banner')
      .getByRole('button', { name: l.locale === 'fa' ? 'باز و بسته کردن نوار کناری' : 'Toggle sidebar' })
    await toggle.click()
    await expect(page.locator('[data-slot="sidebar"][data-state="collapsed"]')).toBeVisible()
    await toggle.click()
    await expect(page.locator('[data-slot="sidebar"][data-state="expanded"]')).toBeVisible()
  })
}

test('the breadcrumb follows the project hierarchy', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod/web/doctor')
  const trail = page.getByRole('navigation', { name: 'Breadcrumb' })
  await expect(trail.getByRole('link', { name: 'Projects' })).toHaveAttribute('href', '/')
  await expect(trail.getByRole('link', { name: 'web' })).toHaveAttribute('href', '/projects/shop/prod/web')
  await expect(trail.locator('[aria-current="page"]')).toHaveText('Doctor')
})

test('the command palette opens with Ctrl+K and goes to a page or a project', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
  await page.keyboard.press('Control+k')
  const palette = page.getByRole('dialog', { name: 'Command palette' })
  await expect(palette).toBeVisible()
  await expect(palette.getByRole('option', { name: /Shop/ })).toBeVisible()
  await expectAccessible(page)
  await page.keyboard.type('audit')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/audit$/)
  await expect(palette).toBeHidden()
  await page.getByRole('button', { name: 'Search…' }).click()
  await page.getByRole('dialog', { name: 'Command palette' }).getByRole('option', { name: /Shop/ }).click()
  await expect(page).toHaveURL(/\/projects\/shop$/)
})

test('the account menu, theme and language menus', async ({ page }) => {
  await prefer(page, 'en', 'light')
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  await page.getByRole('button', { name: 'Theme' }).click()
  await page.getByRole('menuitemradio', { name: 'Dark' }).click()
  await expect(page.locator('html')).toHaveClass(/\bdark\b/)
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
  await page.getByRole('button', { name: 'Language' }).click()
  await page.getByRole('menuitemradio', { name: 'فارسی' }).click()
  await expect(page.locator('html')).toHaveAttribute('dir', 'rtl')
  await page.getByRole('button', { name: 'منوی حساب کاربری' }).click()
  const menu = page.getByRole('menu')
  await expect(menu.getByRole('menuitem', { name: 'حساب کاربری' })).toBeVisible()
  await expect(menu.getByRole('menuitem', { name: 'خروج' })).toBeVisible()
  await expectAccessible(page)
})

test('the skip link moves focus to the page content', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
  await page.keyboard.press('Tab')
  const skip = page.getByRole('link', { name: 'Skip to content' })
  await expect(skip).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.locator('main#content')).toBeFocused()
})

// Kuben serves the console with a strict policy (no 'unsafe-inline'; see
// crates/kuben-api/src/web.rs, which fixtures.ts reads): every page, and the
// overlays that lock scrolling or bring their own styles, must pass it.
test.describe('content security policy', () => {
  const pages = [
    '/',
    '/team',
    '/tokens',
    '/incidents',
    '/webhooks',
    '/domains',
    '/audit',
    '/account',
    '/projects/shop',
    '/projects/shop/prod',
    '/projects/shop/prod/web',
    '/projects/shop/prod/web/doctor',
  ]

  for (const path of pages) {
    test(`no violation on ${path}, with the palette and a menu open`, async ({ page }) => {
      const csp = await watchCsp(page)
      await mockApi(page)
      const response = await page.goto(path)
      expect(response?.headers()['content-security-policy']).toContain("style-src 'self'")
      await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
      await page.keyboard.press('Control+k')
      await expect(page.getByRole('dialog', { name: 'Command palette' })).toBeVisible()
      await page.keyboard.press('Escape')
      await page.getByRole('button', { name: 'Theme' }).click()
      await expect(page.getByRole('menu')).toBeVisible()
      await page.keyboard.press('Escape')
      await page.getByRole('banner').getByRole('button', { name: 'Toggle sidebar' }).click()
      await expect(page.locator('[data-slot="sidebar"][data-state="collapsed"]')).toBeVisible()
      expect(csp).toEqual([])
    })
  }

  test('no violation in the phone drawer', async ({ page }) => {
    const csp = await watchCsp(page)
    await page.setViewportSize({ width: 390, height: 844 })
    await mockApi(page)
    await page.goto('/projects/shop/prod/web')
    await page.getByRole('banner').getByRole('button', { name: 'Toggle sidebar' }).click()
    await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
    expect(csp).toEqual([])
  })

  for (const path of ['/login', '/status/shop']) {
    test(`no violation on ${path} (outside the shell)`, async ({ page }) => {
      const csp = await watchCsp(page)
      await mockApi(page, { signedIn: false })
      const response = await page.goto(path)
      expect(response?.headers()['content-security-policy']).toContain("style-src 'self'")
      await expect(page.locator('#root')).not.toBeEmpty()
      expect(csp).toEqual([])
    })
  }
})
