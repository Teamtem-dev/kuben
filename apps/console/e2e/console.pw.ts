import { expect, test } from '@playwright/test'
import { expectAccessible, prefer, watchCsp } from './checks'
import { mockApi } from './fixtures'

const locales = [
  {
    locale: 'en',
    dir: 'ltr',
    signIn: 'Sign in to Kuben',
    setup: 'Set up Kuben',
    deployments: 'Deployments',
    logs: 'Logs',
    overview: 'Overview',
    settings: 'Settings',
    previous: 'Previous container',
    doctor: 'Doctor',
  },
  {
    locale: 'fa',
    dir: 'rtl',
    signIn: 'ورود به کوبن',
    setup: 'راه‌اندازی کوبن',
    deployments: 'استقرارها',
    logs: 'لاگ‌ها',
    overview: 'نمای کلی',
    settings: 'تنظیمات',
    previous: 'container قبلی',
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

      test('first-run setup page', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page, { signedIn: false, setupNeeded: true })
        await page.goto('/setup')
        await expect(page.getByRole('heading', { name: l.setup })).toBeVisible()
        await expect(page.locator('input[name="org_name"]')).toBeVisible()
        await expect(page.locator('input[name="password"]')).toHaveAttribute('minlength', '12')
        await expectAccessible(page)
        expect(csp).toEqual([])
      })

      test('app page: overview, deployment timeline and live logs in their tabs', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/projects/shop/prod/web')
        await expect(page.getByRole('tab', { name: l.overview, selected: true })).toBeVisible()
        await expect(page.getByText('web-web-7d9c-x2x9q', { exact: true })).toBeVisible()
        await expectAccessible(page)

        await page.getByRole('tab', { name: l.deployments }).click()
        await expect(page).toHaveURL(/\?tab=deployments$/)
        await expect(page.getByRole('heading', { name: l.deployments })).toBeVisible()
        await expect(page.getByText('ghcr.io/acme/web@sha256:0123', { exact: false })).toBeVisible()
        await expectAccessible(page)

        await page.getByRole('tab', { name: l.logs }).click()
        await expect(page).toHaveURL(/\?tab=logs$/)
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
        await page.goto('/projects/shop/prod/web?tab=logs')
        await page.getByRole('radio', { name: l.previous }).click()
        await expect(page.locator('pre[dir="ltr"]')).toContainText('previous crash: out of memory')
      })

      test('app settings: deleting asks for the name in a dialog', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/projects/shop/prod/web?tab=settings')
        await expect(page.getByRole('tab', { name: l.settings, selected: true })).toBeVisible()
        await page.getByRole('button', { name: l.locale === 'fa' ? 'حذف اپ' : 'Delete app' }).click()
        const dialog = page.getByRole('alertdialog')
        await expect(dialog).toBeVisible()
        const confirm = dialog.getByRole('button', { name: l.locale === 'fa' ? 'حذف اپ' : 'Delete app' })
        await expect(confirm).toBeDisabled()
        await dialog.getByRole('textbox').fill('web')
        await expect(confirm).toBeEnabled()
        await expectAccessible(page)
        await page.keyboard.press('Escape')
        await expect(dialog).toBeHidden()
        expect(csp).toEqual([])
      })

      test('projects, a project, an environment', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/')
        const main = page.getByRole('main')
        await main.getByRole('link', { name: /Shop/ }).click()
        await expect(page).toHaveURL(/\/projects\/shop$/)
        await expect(page.getByRole('heading', { level: 1, name: 'Shop' })).toBeVisible()
        await expectAccessible(page)
        await main.getByRole('link', { name: /prod/ }).click()
        await expect(page).toHaveURL(/\/projects\/shop\/prod$/)
        await expect(main.getByRole('link', { name: /web/ })).toHaveAttribute(
          'href',
          '/projects/shop/prod/web',
        )
        await expectAccessible(page)
        expect(csp).toEqual([])
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

test('the account page: profile, password and a way to the API tokens', async ({ page }) => {
  await mockApi(page)
  await page.goto('/account')
  await expect(page.getByRole('heading', { level: 1, name: 'Account' })).toBeVisible()
  await expect(page.getByText('owner@example.com', { exact: true }).first()).toBeVisible()
  await expect(page.getByLabel('New password', { exact: true })).toHaveAttribute('minlength', '12')
  await page.getByRole('link', { name: 'Manage API tokens' }).click()
  await expect(page).toHaveURL(/\/tokens$/)
  const table = page.getByRole('table')
  await expect(table.getByRole('cell', { name: /github-actions/ })).toBeVisible()
  await expect(table.getByRole('button', { name: 'Revoke' })).toBeVisible()
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
// go/hub/internal/api/web/web.go, which fixtures.ts reads): every page, and the
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
    '/projects/shop/prod/web?tab=deployments',
    '/projects/shop/prod/web?tab=releases',
    '/projects/shop/prod/web?tab=logs',
    '/projects/shop/prod/web?tab=settings',
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

  test('no violation with a form dialog open', async ({ page }) => {
    const csp = await watchCsp(page)
    await mockApi(page)
    await page.goto('/')
    await page.getByRole('main').getByRole('button', { name: 'New project' }).click()
    const dialog = page.getByRole('dialog', { name: 'New project' })
    await expect(dialog).toBeVisible()
    await expect(dialog.locator('input[name="name"]')).toBeFocused()
    await expectAccessible(page)
    await dialog.getByRole('button', { name: 'Cancel' }).click()
    await expect(dialog).toBeHidden()
    expect(csp).toEqual([])
  })

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
