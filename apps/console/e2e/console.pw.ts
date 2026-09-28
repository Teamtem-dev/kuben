import { expect, test } from '@playwright/test'
import { expectAccessible, watchCsp } from './checks'
import {
  audit,
  awaitingRun,
  buildLogLines,
  builds,
  builtDigest,
  gitConnections,
  mockApi,
  orgRegistries,
  planHash,
  prefer,
  rotatedWebhookSecret,
  webhookSecret,
} from './fixtures'

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
        '/settings/integrations',
        '/settings/registries',
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

test('deployments: a run waiting for approval is decided with the plan hash it shows', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page, { approval: 'decide' })
  await page.goto('/projects/shop/prod/web?tab=deployments')
  await expect(page.getByText('Deployments waiting for approval: 1')).toBeVisible()
  const panel = page.getByRole('region', { name: 'Waiting for approval' })
  await expect(panel.getByText('1 of 2 approvals')).toBeVisible()
  await expect(panel.getByText('carol@example.com', { exact: true })).toBeVisible()
  await expect(panel.getByText(planHash, { exact: true })).toBeVisible()
  await expect(panel.getByText('the pods may not fit on the nodes')).toBeVisible()
  await expect(panel.getByText('checked the migration')).toBeVisible()
  await expectAccessible(page)

  // A refusal keeps the dialog open with the API's problem text.
  await panel.getByRole('button', { name: 'Reject' }).click()
  const reject = page.getByRole('alertdialog', { name: `Reject revision ${awaitingRun.generation}` })
  await reject.getByRole('button', { name: 'Reject' }).click()
  await expect(reject.getByText('the deployment was already decided')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(reject).toBeHidden()

  await panel.getByRole('button', { name: 'Approve' }).click()
  const approve = page.getByRole('alertdialog', { name: `Approve revision ${awaitingRun.generation}` })
  await expect(approve.getByText(planHash, { exact: true })).toBeVisible()
  await approve.getByLabel('Comment (optional)').fill('ship it')
  await expectAccessible(page)
  const sent = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith(`/deployments/${awaitingRun.run}/approve`),
  )
  await approve.getByRole('button', { name: 'Approve' }).click()
  expect((await sent).postDataJSON()).toEqual({ planHash, comment: 'ship it' })
  await expect(approve).toBeHidden()
  await expect(panel.getByText('Your decision was recorded.')).toBeVisible()
  expect(csp).toEqual([])
})

test('deployments: someone who cannot decide sees the approval without the buttons', async ({ page }) => {
  await mockApi(page, { approval: 'watch' })
  await page.goto('/projects/shop/prod/web?tab=deployments')
  const panel = page.getByRole('region', { name: 'Waiting for approval' })
  await expect(panel.getByText('1 of 2 approvals')).toBeVisible()
  await expect(panel.getByText(/You cannot decide on this deployment/)).toBeVisible()
  await expect(panel.getByRole('button', { name: 'Approve' })).toHaveCount(0)
  await expect(panel.getByRole('button', { name: 'Reject' })).toHaveCount(0)
})

test('builds: a Git-sourced app lists its builds, cancels a running one and shows one in detail', async ({
  page,
}) => {
  const csp = await watchCsp(page)
  await mockApi(page, { gitSource: true })
  await page.goto('/projects/shop/prod/web')
  await page.getByRole('tab', { name: 'Builds' }).click()
  await expect(page).toHaveURL(/\?tab=builds$/)
  const table = page.getByRole('table', { name: 'Builds' })
  const failed = table.getByRole('row', { name: /bbbbbbb/ })
  await expect(failed.getByText('Out of memory')).toBeVisible()
  await expect(failed.getByText('the build used more than 4 GiB')).toBeVisible()
  await expect(failed.getByRole('button', { name: 'Cancel build' })).toHaveCount(0)
  await expectAccessible(page)

  const running = table.getByRole('row', { name: /aaaaaaa/ })
  await expect(running.getByText('Building')).toBeVisible()
  await running.getByRole('button', { name: 'Cancel build' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Cancel build 0190f3c6' })
  await expectAccessible(page)
  const cancelled = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith(`/builds/${builds[0]?.id}/cancel`),
  )
  await confirm.getByRole('button', { name: 'Cancel build' }).click()
  await cancelled
  await expect(confirm).toBeHidden()

  const done = builds[2]
  await table.getByRole('link', { name: `Details of build ${done?.id}` }).click()
  await expect(page).toHaveURL(new RegExp(`tab=builds&build=${done?.id}`))
  await expect(page.getByRole('heading', { name: 'Build 0190f3c6' })).toBeVisible()
  await expect(page.getByText(done?.commit ?? '', { exact: true })).toBeVisible()
  await expect(page.getByText('Scan gate: pass')).toBeVisible()
  await expect(page.getByText('0 critical, 1 high, 3 medium, 7 low')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Download SBOM' })).toHaveAttribute(
    'href',
    `/api/v1/projects/shop/environments/prod/apps/web/sbom/${encodeURIComponent(builtDigest)}`,
  )
  await expectAccessible(page)
  await page.getByRole('link', { name: 'All builds' }).click()
  await expect(page).toHaveURL(/\?tab=builds$/)
  expect(csp).toEqual([])
})

test('builds: only a Git-sourced app has the tab', async ({ page }) => {
  await mockApi(page)
  await page.goto('/projects/shop/prod/web?tab=builds')
  await expect(page.getByRole('tab', { name: 'Overview', selected: true })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Builds' })).toHaveCount(0)
})

test('builds and approvals in Persian, right to left', async ({ page }) => {
  await prefer(page, 'fa', 'light')
  await mockApi(page, { gitSource: true, approval: 'decide' })
  await page.goto('/projects/shop/prod/web?tab=builds')
  await expect(page.locator('html')).toHaveAttribute('dir', 'rtl')
  await expect(page.getByRole('tab', { name: 'ساخت‌ها', selected: true })).toBeVisible()
  await expect(page.getByRole('table', { name: 'ساخت‌ها' }).getByText('کمبود حافظه')).toBeVisible()
  await expectAccessible(page)
  await page.getByRole('tab', { name: 'استقرارها' }).click()
  const panel = page.getByRole('region', { name: 'در انتظار تأیید' })
  await expect(panel.getByRole('button', { name: 'تأیید' })).toBeVisible()
  await expect(panel.getByRole('button', { name: 'رد' })).toBeVisible()
  await expectAccessible(page)
})

test('integrations: connections with their webhook, a token tested before saving, repositories', async ({
  page,
}) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/settings/integrations')
  await expect(page.getByRole('heading', { level: 1, name: 'Integrations' })).toBeVisible()
  await expect(
    page.getByRole('navigation', { name: 'Breadcrumb' }).getByRole('link', { name: 'Settings' }),
  ).toBeVisible()
  const table = page.getByRole('table', { name: 'Git connections' })
  const gitlab = table.getByRole('row', { name: /gitlab-acme/ })
  await expect(gitlab.getByText('acme-bot')).toBeVisible()
  await expect(gitlab.getByText(`http://kuben.test${gitConnections[0]?.webhookUrl}`)).toBeVisible()
  await expect(gitlab.getByText(/Settings → Webhooks/)).toBeVisible()
  const codeberg = table.getByRole('row', { name: /codeberg/ })
  await expect(codeberg.getByText(/Forgejo ·/)).toBeVisible()
  await expect(codeberg.getByText('the token was revoked (401)')).toBeVisible()
  // The GitHub App's installations are on the same page.
  await expect(page.getByText('#4242')).toBeVisible()
  await expectAccessible(page)

  // A token is tested before it can be saved; a change asks for a new test.
  await page.getByRole('button', { name: 'Add connection' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add connection' })
  await dialog.getByRole('radio', { name: /Gitea/ }).check({ force: true })
  await expect(dialog.getByLabel('URL')).toHaveValue('')
  await dialog.getByRole('radio', { name: /GitLab/ }).check({ force: true })
  await expect(dialog.getByLabel('URL')).toHaveValue('https://gitlab.com')
  await expect(dialog.getByLabel('Name')).toHaveValue('gitlab')
  const save = dialog.getByRole('button', { name: 'Save connection' })
  await dialog.getByLabel('Access token').fill('glpat-secret')
  await expect(save).toBeDisabled()
  const tested = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/git/connections/test'),
  )
  await dialog.getByRole('button', { name: 'Test', exact: true }).click()
  expect((await tested).postDataJSON()).toEqual({
    provider: 'gitlab',
    name: 'gitlab',
    token: 'glpat-secret',
    baseUrl: 'https://gitlab.com',
  })
  await expect(dialog.getByRole('status').getByText('acme-bot')).toBeVisible()
  await expect(dialog.getByText('read_repository')).toBeVisible()
  await expect(save).toBeEnabled()
  await expectAccessible(page)
  await dialog.getByLabel('Name').fill('gitlab-2')
  await expect(save).toBeDisabled()
  await dialog.getByRole('button', { name: 'Test', exact: true }).click()
  const created = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/git/connections'),
  )
  await save.click()
  expect((await created).postDataJSON()).toMatchObject({ provider: 'gitlab', name: 'gitlab-2' })
  // The webhook secret is shown this once, with the address and where both go.
  await expect(dialog.getByRole('alert').getByText(/shown only this once/)).toBeVisible()
  await expect(dialog.getByText(webhookSecret, { exact: true })).toBeVisible()
  await expect(
    dialog.getByText('http://kuben.test/api/v1/webhooks/gitlab/0190f3c6-0000-7000-8000-0000000000g3'),
  ).toBeVisible()
  await expect(dialog.getByText(/Secret token, and tick Push events/)).toBeVisible()
  await expectAccessible(page)
  await dialog.getByRole('button', { name: 'Done' }).click()
  await expect(dialog).toBeHidden()

  // Rotating the secret asks first, then shows the new one once.
  await gitlab.getByRole('button', { name: 'Rotate webhook secret' }).click()
  const rotate = page.getByRole('dialog', { name: 'Rotate the webhook secret of gitlab-acme' })
  await expect(rotate.getByText(/stops working at once/)).toBeVisible()
  const rotated = page.waitForRequest(
    (r) =>
      r.method() === 'POST' && r.url().endsWith(`/git/connections/${gitConnections[0]?.id}/webhook-secret`),
  )
  await rotate.getByRole('button', { name: 'Rotate webhook secret' }).click()
  await rotated
  await expect(rotate.getByText(rotatedWebhookSecret, { exact: true })).toBeVisible()
  await rotate.getByRole('button', { name: 'Done' }).click()
  await expect(rotate).toBeHidden()

  // Repositories page by page, and a repository's branches.
  await gitlab.getByRole('button', { name: 'Repositories' }).click()
  const browse = page.getByRole('dialog', { name: 'Repositories of gitlab-acme' })
  await expect(browse.getByRole('radio', { name: /acme\/web/ })).toBeVisible()
  await browse.getByRole('button', { name: 'Next page' }).click()
  await browse.getByRole('radio', { name: /acme\/docs/ }).check()
  await expect(browse.getByLabel('Branch')).toHaveValue('main')
  await expectAccessible(page)
  await browse.getByRole('button', { name: 'Close' }).click()

  // A connection in use cannot be deleted; the dialog says why.
  await gitlab.getByRole('button', { name: 'Delete' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Delete gitlab-acme' })
  await confirm.getByRole('button', { name: 'Delete' }).click()
  await expect(confirm.getByText(/App sources still read through this connection/)).toBeVisible()
  expect(csp).toEqual([])
})

test('registries: a preset fills the server, the login is tested first, rotated and deleted', async ({
  page,
}) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/settings/registries')
  const table = page.getByRole('table', { name: 'Registry logins' })
  const row = table.getByRole('row', { name: /ghcr/ })
  await expect(row.getByText('GitHub Packages')).toBeVisible()
  await expect(row.getByText('Login works')).toBeVisible()
  await expectAccessible(page)

  await page.getByRole('button', { name: 'Add registry' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add registry' })
  await dialog.getByText('Harbor', { exact: true }).click()
  await expect(dialog.getByLabel('Server')).toHaveValue('')
  await expect(dialog.getByText('A robot account (robot$…)')).toBeVisible()
  await dialog.getByText('Docker Hub', { exact: true }).click()
  await expect(dialog.getByLabel('Server')).toHaveValue('docker.io')
  await expect(dialog.getByRole('link', { name: 'How to create a token for Docker Hub' })).toBeVisible()
  await dialog.getByLabel('Username').fill('acme')
  await dialog.getByLabel('Password or token').fill('dckr_pat_x')
  const save = dialog.getByRole('button', { name: 'Save login' })
  await expect(save).toBeDisabled()
  const tested = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/registries/test'),
  )
  await dialog.getByRole('button', { name: 'Test', exact: true }).click()
  expect((await tested).postDataJSON()).toEqual({
    preset: 'dockerhub',
    name: 'dockerhub',
    username: 'acme',
    password: 'dckr_pat_x',
  })
  await expect(dialog.getByRole('status').getByText('Login works')).toBeVisible()
  await expectAccessible(page)
  await save.click()
  await expect(dialog).toBeHidden()

  await row.getByRole('button', { name: 'Test' }).click()
  await expect(page.getByText('unauthorized: the token expired')).toBeVisible()

  await row.getByRole('button', { name: 'Rotate' }).click()
  const rotate = page.getByRole('dialog', { name: 'Rotate ghcr' })
  await rotate.getByLabel('New password or token').fill('ghp_new')
  const put = page.waitForRequest(
    (r) => r.method() === 'PUT' && r.url().endsWith(`/api/v1/registries/${orgRegistries[0]?.id}`),
  )
  await rotate.getByRole('button', { name: 'Rotate' }).click()
  expect((await put).postDataJSON()).toEqual({ password: 'ghp_new' })
  await expect(rotate).toBeHidden()

  await row.getByRole('button', { name: 'Delete' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Delete ghcr' })
  const removed = page.waitForRequest((r) => r.method() === 'DELETE')
  await confirm.getByRole('button', { name: 'Delete' }).click()
  await removed
  await expect(confirm).toBeHidden()
  expect(csp).toEqual([])
})

test('app settings: a repository is connected through a Git connection', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page)
  await page.goto('/projects/shop/prod/web?tab=settings')
  const card = page.getByRole('heading', { name: 'Git source' })
  await expect(card).toBeVisible()
  await page.getByRole('button', { name: 'Connect repository' }).click()
  const dialog = page.getByRole('dialog', { name: 'Connect repository' })
  await expect(dialog.getByLabel('Through')).toHaveValue(`connection:${gitConnections[0]?.id}`)
  await dialog.getByRole('radio', { name: /acme\/web/ }).check()
  await expect(dialog.getByLabel('Branch')).toHaveValue('main')
  await dialog.getByLabel('Branch').selectOption('develop')
  await dialog.getByLabel('Image repository').fill('registry.example.com/acme/web')
  await expectAccessible(page)
  const put = page.waitForRequest((r) => r.method() === 'PUT' && r.url().endsWith('/apps/web/source'))
  await dialog.getByRole('button', { name: 'Save and build' }).click()
  expect((await put).postDataJSON()).toEqual({
    repository: 'acme/web',
    branch: 'develop',
    strategy: 'auto',
    imageRepository: 'registry.example.com/acme/web',
    connection: gitConnections[0]?.id,
  })
  await expect(dialog).toBeHidden()

  // Through the GitHub App, the repository and branch are typed; a malformed repository is said so.
  await page.getByRole('button', { name: 'Change source' }).click()
  const change = page.getByRole('dialog', { name: 'Change source' })
  await change.getByLabel('Through').selectOption('installation:4242')
  await expect(change.getByRole('radio')).toHaveCount(0)
  await change.getByLabel('Repository').fill('just-a-name')
  await change.getByLabel('Branch').fill('main')
  await change.getByLabel('Image repository').fill('registry.example.com/acme/web')
  await change.getByRole('button', { name: 'Save and build' }).click()
  await expect(change.getByRole('alert').getByText(/Not a repository/)).toBeVisible()
  await expect(change.getByLabel('Repository')).toHaveAttribute('aria-invalid', 'true')
  await expectAccessible(page)
  const put2 = page.waitForRequest((r) => r.method() === 'PUT' && r.url().endsWith('/apps/web/source'))
  await change.getByLabel('Repository').fill('https://github.com/acme/web.git')
  await change.getByRole('button', { name: 'Save and build' }).click()
  expect((await put2).postDataJSON()).toMatchObject({ repository: 'acme/web', installationId: 4242 })
  expect(csp).toEqual([])
})

test('builds: build now opens the build, with its stages and the followed log', async ({ page }) => {
  const csp = await watchCsp(page)
  await mockApi(page, { gitSource: true })
  await page.goto('/projects/shop/prod/web?tab=settings')
  await expect(page.getByText('gitlab-acme')).toBeVisible()
  await expect(page.getByText('registry.example.com/acme/web')).toBeVisible()

  await page.getByRole('tab', { name: 'Builds' }).click()
  const triggered = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith('/apps/web/builds'))
  await page.getByRole('button', { name: 'Build now' }).click()
  await triggered
  await expect(page).toHaveURL(new RegExp(`tab=builds&build=${builds[0]?.id}`))
  const stages = page.getByRole('list', { name: 'Stages' })
  await expect(stages.getByRole('listitem')).toHaveCount(4)
  await expect(stages.locator('[aria-current="step"]')).toContainText('Build')
  const log = page.locator('pre[dir="ltr"]')
  for (const line of buildLogLines) await expect(log).toContainText(line)
  await expect(page.getByRole('link', { name: 'Download' })).toHaveAttribute(
    'href',
    `/api/v1/projects/shop/environments/prod/apps/web/builds/${builds[0]?.id}/logs`,
  )
  await page.getByRole('switch', { name: 'Wrap lines' }).click()
  await expectAccessible(page)

  // A settled build shows its kept log.
  await page.goto(`/projects/shop/prod/web?tab=builds&build=${builds[2]?.id}`)
  await expect(page.locator('pre[dir="ltr"]')).toContainText('railpack plan: node 22')
  await expect(page.getByRole('list', { name: 'Stages' }).getByText('Skipped')).toBeVisible()
  await expectAccessible(page)
  expect(csp).toEqual([])
})

test('builds: a build delta on the stream updates the list without a refetch', async ({ page }) => {
  await mockApi(page, { gitSource: true, buildDelta: true })
  await page.goto('/projects/shop/prod/web?tab=builds')
  const running = page.getByRole('table', { name: 'Builds' }).getByRole('row', { name: /aaaaaaa/ })
  await expect(running.getByText('Succeeded')).toBeVisible()
  await expect(running.getByRole('button', { name: 'Cancel build' })).toHaveCount(0)
})

test('integrations and builds in Persian, right to left', async ({ page }) => {
  await prefer(page, 'fa', 'dark')
  await mockApi(page, { gitSource: true })
  await page.goto('/settings/integrations')
  await expect(page.locator('html')).toHaveAttribute('dir', 'rtl')
  await expect(page.getByRole('heading', { level: 1, name: 'یکپارچه‌سازی‌ها' })).toBeVisible()
  await expect(page.getByRole('table', { name: 'اتصال‌های Git' })).toBeVisible()
  await expectAccessible(page)
  await page.goto(`/projects/shop/prod/web?tab=builds&build=${builds[0]?.id}`)
  await expect(page.getByRole('list', { name: 'مرحله‌ها' })).toBeVisible()
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
    '/settings/integrations',
    '/settings/registries',
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
