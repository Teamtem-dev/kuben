import { expect, test } from '@playwright/test'
import { expectAccessible, watchCsp } from './checks'
import { audit, mockApi, prefer } from './fixtures'

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
    evidence: 'Evidence path',
    home: 'Home',
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
    evidence: 'مسیر شواهد',
    home: 'خانه',
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

      test('home: summary, open incidents, recent deployments and health', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page)
        await page.goto('/')
        await expect(page.getByRole('heading', { level: 1, name: l.home })).toBeVisible()
        const main = page.getByRole('main')
        await expect(main.getByText('web in shop/prod failed to deploy', { exact: true })).toBeVisible()
        // The incident's place and the deployment's target both link to the app.
        await expect(main.getByRole('link', { name: 'shop/prod/web' })).toHaveCount(2)
        await expect(main.getByText('hooks.example.com: 502', { exact: true })).toBeVisible()
        await expect(main.locator('[data-slot="skeleton"]')).toHaveCount(0)
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
        const path = page.getByRole('list', { name: l.evidence })
        await expect(path).toBeVisible()
        await expect(path.getByRole('heading', { level: 3 })).toHaveCount(4)
        await expect(path.getByRole('link', { name: 'DNS' })).toHaveAttribute('href', '#evidence-dns')
        await expectAccessible(page)
        expect(csp).toEqual([])
      })

      for (const path of [
        '/incidents',
        '/webhooks',
        '/domains',
        '/team',
        '/audit',
        '/settings',
        '/projects/shop',
        '/projects/shop/prod',
      ]) {
        test(`${path}: one heading, accessible, no CSP violation`, async ({ page }) => {
          const csp = await watchCsp(page)
          await prefer(page, l.locale, theme)
          await mockApi(page)
          await page.goto(path)
          await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1)
          await expect(page.getByRole('main').getByRole('status')).toHaveCount(0)
          await expectAccessible(page)
          expect(csp).toEqual([])
        })
      }

      test('public status page', async ({ page }) => {
        const csp = await watchCsp(page)
        await prefer(page, l.locale, theme)
        await mockApi(page, { signedIn: false })
        await page.goto('/status/shop')
        await expect(page.getByRole('heading', { level: 1, name: 'Shop status' })).toBeVisible()
        await expect(page.getByText('worker', { exact: true }).first()).toBeVisible()
        await expectAccessible(page)
        expect(csp).toEqual([])
      })
    })
  }
}

test('incidents: an open one can be acknowledged or resolved; resolved ones are a switch away', async ({
  page,
}) => {
  await mockApi(page)
  await page.goto('/incidents')
  const table = page.getByRole('table', { name: 'Incidents' })
  const row = table.getByRole('row', { name: /web in shop\/prod failed to deploy/ })
  await expect(row).toBeVisible()
  await expect(row.getByRole('link', { name: 'shop/prod/web' })).toHaveAttribute(
    'href',
    '/projects/shop/prod/web',
  )
  await expect(page.getByRole('button', { name: 'Acknowledge' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Resolve' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Runbook' })).toHaveAttribute(
    'href',
    'https://runbooks.example.com/deploy-failed',
  )
  const filter = page.getByRole('switch', { name: 'Show resolved' })
  await expect(filter).not.toBeChecked()
  const refetch = page.waitForRequest((r) => r.url().includes('/api/v1/incidents?all=true'))
  await filter.click()
  await refetch
  await expect(filter).toBeChecked()
})

test('webhooks: add in a dialog, deliveries unfold, delete asks for the name', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/webhooks')
  await expect(page.getByText('https://hooks.example.com/kuben', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Deliveries' }).click()
  await expect(page.getByText('bad gateway', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Hide deliveries' })).toHaveAttribute('aria-expanded', 'true')

  await page.getByRole('main').getByRole('button', { name: 'Add a webhook' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add a webhook' })
  await expect(dialog).toBeVisible()
  const all = dialog.getByRole('checkbox', { name: 'All events' })
  await expect(all).toBeChecked()
  await dialog.getByRole('checkbox', { name: 'build.failed' }).click()
  await expect(all).not.toBeChecked()
  await expectAccessible(page)
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toBeHidden()

  await page.getByRole('button', { name: 'Delete webhook' }).click()
  const confirm = page.getByRole('alertdialog')
  await expect(confirm.getByRole('button', { name: 'Delete webhook' })).toBeDisabled()
  await confirm.getByRole('textbox').fill('ops-pager')
  await expect(confirm.getByRole('button', { name: 'Delete webhook' })).toBeEnabled()
  expect(csp).toEqual([])
})

test('domains: a pending claim shows its TXT record to copy', async ({ page }) => {
  await mockApi(page)
  await page.goto('/domains')
  await expect(page.getByText('_kuben-challenge.example.com', { exact: true })).toBeVisible()
  await expect(page.getByText('kuben-verify=4f1d2c', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy' })).toHaveCount(2)
  await expect(page.getByRole('button', { name: 'Verify' })).toBeVisible()
  await page.getByRole('main').getByRole('button', { name: 'Claim', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Claim' }).getByLabel('Domain')).toBeFocused()
})

test('team: members in a table, your own row locked, invite in a dialog', async ({ page }) => {
  await mockApi(page)
  await page.goto('/team')
  const table = page.getByRole('table')
  await expect(table.getByRole('combobox', { name: 'Role of owner@example.com' })).toBeDisabled()
  await expect(table.getByRole('combobox', { name: 'Role of carol@example.com' })).toHaveValue('developer')
  await expect(table.getByText('invitation pending', { exact: false })).toBeVisible()
  await page.getByRole('main').getByRole('button', { name: 'Invite a member' }).click()
  const dialog = page.getByRole('dialog', { name: 'Invite a member' })
  await expect(dialog.getByLabel('Role')).toHaveValue('developer')
  await expectAccessible(page)
})

test('audit log: the events in a table', async ({ page }) => {
  await mockApi(page)
  await page.goto('/audit')
  const table = page.getByRole('table')
  const row = table.getByRole('row', { name: /app\.update/ })
  await expect(row.getByRole('cell', { name: 'app.update' })).toBeVisible()
  await expect(row.getByRole('cell', { name: 'shop/prod/web' })).toBeVisible()
})

test('data table: pages, a filter, and sorting from the keyboard', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  const [first] = audit.events
  const events = Array.from({ length: 30 }, (_, i) => ({
    ...first,
    seq: 100 + i,
    id: `0190f3c6-0000-7000-8000-${String(i).padStart(12, '0')}`,
    at: (first?.at ?? 0) - i * 60_000,
    action: i % 3 === 0 ? 'deleteSecret' : 'app.update',
  }))
  await page.route('http://kuben.test/api/v1/audit**', (route) =>
    route.fulfill({ json: { events, next_before: null } }),
  )
  await page.goto('/audit')
  const table = page.getByRole('table', { name: 'Audit log' })
  await expect(table.getByRole('row')).toHaveCount(26)
  await expect(page.getByText('30 rows', { exact: true })).toBeVisible()
  const pager = page.getByRole('navigation', { name: 'Audit log: pages' })
  await expect(pager.getByText('Page 1 of 2')).toBeVisible()
  await expect(pager.getByRole('button', { name: 'Previous page' })).toBeDisabled()
  await pager.getByRole('button', { name: 'Next page' }).click()
  await expect(pager.getByText('Page 2 of 2')).toBeVisible()
  await expect(table.getByRole('row')).toHaveCount(6)

  await page.getByRole('searchbox', { name: 'Filter Audit log' }).fill('deletesecret')
  await expect(page.getByText('10 of 30 rows', { exact: true })).toBeVisible()
  await expect(table.getByRole('row')).toHaveCount(11)
  await expect(pager).toBeHidden()

  const when = table.getByRole('columnheader', { name: 'When' })
  const action = table.getByRole('columnheader', { name: 'Action' })
  await expect(when).toHaveAttribute('aria-sort', 'descending')
  await action.getByRole('button').focus()
  await page.keyboard.press('Enter')
  await expect(action).toHaveAttribute('aria-sort', 'ascending')
  await expect(when).not.toHaveAttribute('aria-sort')
  await page.keyboard.press('Space')
  await expect(action).toHaveAttribute('aria-sort', 'descending')
  await page.keyboard.press('Enter')
  await expect(action).not.toHaveAttribute('aria-sort')
  await expectAccessible(page)
  expect(csp).toEqual([])
})

test('controls: freezes and silences, delivery, owners, the emergency rollback', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/projects/shop/prod')
  const card = (title: string) =>
    page.locator('[data-slot="card"]', { has: page.getByRole('heading', { level: 2, name: title }) })
  const freezes = card('Change freezes')
  await expect(freezes.getByText('end-of-quarter close', { exact: true })).toBeVisible()
  await expect(freezes.getByText('In force', { exact: true })).toBeVisible()
  await expect(freezes.getByRole('button', { name: 'Lift' })).toBeVisible()
  const silences = card('Alert silences')
  await expect(silences.getByText('No silences in force.')).toBeVisible()
  await silences.getByRole('button', { name: 'Add a silence' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add a silence' })
  await expect(dialog.getByLabel('Applies to')).toHaveValue('')
  await expect(dialog.getByLabel('Applies to').getByRole('option', { name: 'web' })).toHaveCount(1)
  await expect(dialog.getByLabel('Ends')).not.toHaveValue('')
  await expectAccessible(page)
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toBeHidden()

  await page.goto('/projects/shop')
  await expect(card('Owner').getByLabel('Team or person')).toHaveValue('shop-team')

  await page.goto('/projects/shop/prod/web?tab=settings')
  await expect(card('Owner').getByText('No owner named yet.')).toBeVisible()
  await card('Delivery').getByRole('button', { name: 'Pause delivery' }).click()
  const pause = page.getByRole('dialog', { name: 'Pause delivery' })
  await expect(pause.getByLabel('Reason')).toBeFocused()
  await expectAccessible(page)
  await page.keyboard.press('Escape')
  await expect(pause).toBeHidden()

  await page.goto('/projects/shop/prod/web?tab=releases')
  await page.getByRole('button', { name: 'Roll back now…' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Emergency rollback' })
  const submit = confirm.getByRole('button', { name: 'Roll back now' })
  await expect(submit).toBeDisabled()
  await confirm.getByLabel('Why this cannot wait').fill('checkout is down')
  await expect(submit).toBeEnabled()
  await expectAccessible(page)
  expect(csp).toEqual([])
})

test('settings: single sign-on and CI trust policies', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/settings')
  await expect(page.getByText('Acme SSO', { exact: true })).toBeVisible()
  const table = page.getByRole('table', { name: 'CI trust policies' })
  const row = table.getByRole('row', { name: /shop-deploy/ })
  await expect(row.getByRole('cell', { name: 'shop', exact: true })).toBeVisible()
  await expect(row.getByText('owner@example.com', { exact: true })).toBeVisible()
  await expect(row.getByText('refs/heads/main', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Add a policy' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add a policy' })
  await expect(dialog.getByLabel('Project')).toHaveValue('')
  await dialog.getByLabel('Project').selectOption('shop')
  await expectAccessible(page)
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toBeHidden()
  await row.getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByRole('alertdialog', { name: 'Revoke shop-deploy' })).toBeVisible()
  expect(csp).toEqual([])
})

test('project page: previews, their policy and the status page settings', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/projects/shop')
  await expect(page.getByRole('link', { name: 'pr-42' })).toHaveAttribute('href', '/projects/shop/pr-42')
  await expect(page.getByRole('checkbox', { name: 'Previews for pull requests' })).toBeChecked()
  await expect(page.getByRole('checkbox', { name: 'Also for forks' })).not.toBeChecked()
  await expect(page.getByRole('switch', { name: 'Show closed' })).not.toBeChecked()
  await expect(page.getByLabel('Address')).toHaveValue('shop')
  await expect(page.getByRole('link', { name: 'Open page' })).toHaveAttribute('href', '/status/shop')

  await page.getByRole('button', { name: 'Destroy' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Destroy' })
  await expect(confirm).toContainText('pr-42')
  await expectAccessible(page)
  await confirm.getByRole('button', { name: 'Cancel' }).click()
  await expect(confirm).toBeHidden()
  expect(csp).toEqual([])
})

test('environment page: a detached app and what it left behind', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod')
  await expect(page.getByRole('heading', { name: 'Detached apps' })).toBeVisible()
  await expect(page.getByText('moved to Helm', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Release' })).toBeVisible()
})

test('app page: usage window and detaching behind a dialog', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod/web')
  const timeWindow = page.getByRole('radiogroup', { name: 'Time window' })
  await expect(timeWindow.getByRole('radio', { name: '1 hour' })).toBeChecked()
  await expect(page.getByRole('img', { name: 'CPU over time' })).toBeVisible()

  await page.goto('/projects/shop/prod/web?tab=settings')
  await expect(page.getByRole('heading', { name: 'Automatic image updates' })).toBeVisible()
  await page.getByRole('button', { name: 'Detach from Kuben…' }).click()
  const dialog = page.getByRole('alertdialog', { name: 'Detach this app' })
  const submit = dialog.getByRole('button', { name: 'Detach' })
  await expect(submit).toBeDisabled()
  await dialog.getByLabel('Reason').fill('moving to Helm')
  await dialog.getByLabel(/Type the app name/).fill('web')
  await expect(submit).toBeEnabled()
  await expectAccessible(page)
})

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
  await expect(trail.getByRole('link', { name: 'Home' })).toHaveAttribute('href', '/')
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
// apps/kuben/internal/httpapi/web/web.go, which fixtures.ts reads): every page, and the
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
    '/settings',
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
