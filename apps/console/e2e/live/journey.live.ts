import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Locator, type Page, test } from '@playwright/test'
import { en, fa, type MessageKey } from '../../src/lib/messages'
import { fill } from '../../src/lib/messages/pages'
import { expectAccessible, prefer, watchCsp } from '../checks'

// A user's first hour with Kuben, in a browser, against the real thing: the
// console embedded in `kuben serve`, its Go API and PostgreSQL. No cluster is
// connected, so everything that needs one says so. The strings come from the
// console's catalogue, so a label that changes there changes here.

const ADMIN = { org: 'Live Test', email: 'owner@example.com', password: 'correct horse battery staple' }
const MEMBER = 'developer@example.com'
const PROJECT = { name: 'shop', title: 'Online Shop' }
const ENV = 'staging'
const APP = 'hello'
// Pinned by digest: a tag would be resolved at its registry, a digest is
// accepted as it is. Nothing pulls it (no cluster).
const IMAGE = `docker.io/library/nginx@sha256:${'0123456789abcdef'.repeat(4)}`
const TOKEN = 'live-ci-token'
const NO_CLUSTER = 'no kubernetes cluster configured'

type Messages = Record<MessageKey, string>

/** The setup link as `kuben setup-token` printed it, else the token file in the state directory. */
function setupLink(): { path: string; token?: string } {
  const printed = process.env.CONSOLE_SETUP_LINK?.trim()
  if (printed) {
    const url = new URL(printed)
    const token = new URLSearchParams(url.hash.slice(1)).get('token') ?? undefined
    return { path: `${url.pathname}${url.hash}`, token }
  }
  const stateDir = process.env.KUBEN_SERVER__STATE_DIR
  const file = stateDir ? join(stateDir, 'setup-token') : ''
  const token = file && existsSync(file) ? readFileSync(file, 'utf8').trim() : ''
  return token ? { path: `/setup#token=${token}`, token } : { path: '/setup' }
}

/** Server errors, except the 503 every cluster-backed call answers without a cluster. */
function watchServerErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('response', (response) => {
    const status = response.status()
    if (status >= 500 && status !== 503) {
      errors.push(`${status} ${response.request().method()} ${new URL(response.url()).pathname}`)
    }
  })
  return errors
}

async function open(page: Page, locale: 'en' | 'fa', theme: 'dark' | 'light') {
  const csp = await watchCsp(page)
  const errors = watchServerErrors(page)
  await prefer(page, locale, theme)
  return { csp, errors }
}

/** The card (a section with an h2 or a data-slot="card") titled `name`. */
function card(page: Page, name: string | RegExp): Locator {
  return page
    .getByRole('heading', typeof name === 'string' ? { name, exact: true } : { name })
    .locator('xpath=ancestor::*[@data-slot="card" or self::section][1]')
}

async function signIn(page: Page, m: Messages) {
  await page.goto('/login')
  await expect(page.getByRole('heading', { name: m['login.title'] })).toBeVisible()
  await page.getByLabel(m['login.email'], { exact: true }).fill(ADMIN.email)
  await page.getByLabel(m['login.password'], { exact: true }).fill(ADMIN.password)
  await page.getByRole('button', { name: m['login.submit'], exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: m['nav.home'] })).toBeVisible()
}

async function signOut(page: Page, m: Messages) {
  await page.getByRole('button', { name: m['shell.userMenu'] }).click()
  await page.getByRole('menuitem', { name: m['shell.signOut'] }).click()
}

function nav(page: Page, m: Messages) {
  return page.getByRole('navigation', { name: m['nav.label'] })
}

test.describe.configure({ mode: 'serial' })

test('first run: the admin account is created on /setup, then signs out and in', async ({ page }) => {
  const { csp, errors } = await open(page, 'en', 'dark')
  const status = (await (await page.request.get('/api/v1/setup')).json()) as {
    needed: boolean
    token_required: boolean
    secure: boolean
  }
  expect(status.needed, 'a fresh server has no admin yet').toBe(true)
  expect(status.secure).toBe(true)
  const link = setupLink()
  expect(
    status.token_required && !link.token,
    'CONSOLE_SETUP_LINK carries the token the server asks for',
  ).toBe(false)

  // Every page leads to the setup until the first admin exists.
  await page.goto('/')
  await expect(page).toHaveURL(/\/setup$/)

  // Opened afresh, as from the terminal: a fragment-only change would not load the page again.
  await page.goto('about:blank')
  await page.goto(link.path)
  await expect(page.getByRole('heading', { name: en['setup.title'] })).toBeVisible()
  // The token is taken out of the address bar once read.
  await expect(page).toHaveURL(/\/setup$/)
  await expect(page.getByLabel(en['setup.token'], { exact: true })).toHaveCount(0)
  await expectAccessible(page)

  await page.getByLabel(en['setup.organization'], { exact: true }).fill(ADMIN.org)
  await page.getByLabel(en['login.email'], { exact: true }).fill(ADMIN.email)
  await page.getByLabel(en['login.password'], { exact: true }).fill(ADMIN.password)
  await page.getByRole('button', { name: en['setup.submit'] }).click()

  await expect(page.getByRole('heading', { level: 1, name: en['nav.home'] })).toBeVisible()
  await expect(page.getByText(en['projects.empty'])).toBeVisible()
  await expectAccessible(page)

  await signOut(page, en)
  await expect(page.getByRole('heading', { name: en['login.title'] })).toBeVisible()
  // Setup happens once; afterwards its page leads to the sign-in.
  await page.goto('/setup')
  await expect(page).toHaveURL(/\/login$/)
  await expectAccessible(page)

  await signIn(page, en)
  expect(csp).toEqual([])
  expect(errors).toEqual([])
})

test('a project, an environment and an image app, without a cluster', async ({ page }) => {
  const { csp, errors } = await open(page, 'en', 'dark')
  await signIn(page, en)

  await page.getByRole('button', { name: en['projects.new'] }).click()
  await page.getByLabel(en['projects.name'], { exact: true }).fill(PROJECT.name)
  await page.getByLabel(en['projects.displayName'], { exact: true }).fill(PROJECT.title)
  await page.getByLabel(en['projects.description'], { exact: true }).fill('Made by the live console test')
  await page.getByRole('button', { name: en['projects.create'] }).click()
  await expect(page).toHaveURL(new RegExp(`/projects/${PROJECT.name}$`))
  await expect(page.getByRole('heading', { level: 1, name: PROJECT.title })).toBeVisible()
  await expect(page.getByText(en['project.empty'])).toBeVisible()

  await page.getByRole('button', { name: en['project.newEnvironment'] }).click()
  await page.getByLabel(en['projects.name'], { exact: true }).fill(ENV)
  await page.getByLabel(en['project.type'], { exact: true }).selectOption('standard')
  await page.getByRole('button', { name: en['project.createEnvironment'] }).click()
  const environment = page.getByRole('link', { name: new RegExp(ENV) })
  await expect(environment).toBeVisible()
  await expect(environment).toContainText(`kb-${PROJECT.name}-${ENV}`)
  await expectAccessible(page)

  await environment.click()
  await expect(page.getByRole('heading', { level: 1, name: new RegExp(ENV) })).toBeVisible()
  await expect(page.getByText(en['environment.empty'])).toBeVisible()
  await page.getByRole('button', { name: en['environment.deployApp'] }).click()
  // The deploy form is the one with a port; the registry login form also has a name.
  const deploy = page
    .locator('form')
    .filter({ has: page.getByLabel(en['environment.port'], { exact: true }) })
  await deploy.getByLabel(en['projects.name'], { exact: true }).fill(APP)
  await deploy.getByLabel(en['environment.image'], { exact: true }).fill(IMAGE)
  await deploy.getByLabel(en['environment.port'], { exact: true }).fill('80')
  await deploy.getByRole('button', { name: en['ui.deploy'], exact: true }).click()
  await expect(deploy).toBeHidden()
  const app = page.getByRole('link', { name: new RegExp(APP) })
  await expect(app).toContainText('nginx')
  await expectAccessible(page)

  // The app page: the run and the release are in PostgreSQL; pods and logs
  // come from the cluster, which the API says is not there.
  await app.click()
  await expect(page.getByRole('heading', { level: 1, name: new RegExp(APP) })).toBeVisible()
  await expect(card(page, fill(en['app.pods'], { count: 0 }))).toContainText(en['app.noPods'])

  await page.getByRole('tab', { name: en['deployments.title'] }).click()
  const deployments = card(page, en['deployments.title'])
  await expect(deployments).toContainText(new RegExp(`${en['deployments.revision']} \\d+`))
  await expect(deployments).toContainText(en['deployments.reason.deploy'])

  await page.getByRole('tab', { name: en['app.releases'] }).click()
  await expect(card(page, en['app.releases'])).toContainText(en['app.current'])

  await page.getByRole('tab', { name: en['logs.title'] }).click()
  const logs = card(page, new RegExp(`^${en['logs.title']}`))
  // Following needs the cluster: the stream is refused (503) and ends.
  await expect(logs.getByRole('status')).toHaveText(en['logs.ended'])
  await expectAccessible(page)
  await logs.getByRole('radio', { name: en['logs.previous'] }).click()
  await expect(logs.getByRole('alert')).toContainText(NO_CLUSTER)

  expect(csp).toEqual([])
  expect(errors).toEqual([])
})

test('team, API tokens, audit log and sign out', async ({ page }) => {
  const { csp, errors } = await open(page, 'en', 'light')
  await signIn(page, en)

  await nav(page, en).getByRole('link', { name: en['nav.team'] }).click()
  await expect(page.getByRole('heading', { level: 1, name: en['nav.team'] })).toBeVisible()
  const members = card(page, fill(en['team.members'], { count: 1 }))
  await expect(members).toContainText(ADMIN.email)
  await expect(members).toContainText(en['team.you'])
  const ownRole = page.getByLabel(fill(en['team.roleOf'], { email: ADMIN.email }), { exact: true })
  await expect(ownRole).toHaveValue('owner')
  await expect(ownRole).toBeDisabled()
  await expectAccessible(page)

  await page.getByRole('button', { name: en['team.invite'] }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel(en['login.email'], { exact: true }).fill(MEMBER)
  await dialog.getByLabel(en['team.role'], { exact: true }).selectOption('developer')
  await dialog.getByRole('button', { name: en['team.inviteButton'], exact: true }).click()
  await expect(page.getByRole('status')).toContainText(`${en['team.tempPasswordFor']} ${MEMBER}`)
  await expect(card(page, fill(en['team.members'], { count: 2 }))).toContainText(en['team.invitationPending'])

  await nav(page, en).getByRole('link', { name: en['nav.tokens'] }).click()
  await expect(page.getByRole('heading', { level: 1, name: en['nav.tokens'] })).toBeVisible()
  await expect(page.getByText(en['tokens.empty'])).toBeVisible()
  const create = card(page, en['tokens.create'])
  await create.getByLabel(en['projects.name'], { exact: true }).fill(TOKEN)
  await create.getByRole('button', { name: en['tokens.createButton'], exact: true }).click()
  await expect(create.getByRole('status')).toContainText('kbn_pat_')
  const token = page.getByRole('row').filter({ hasText: TOKEN })
  await expect(token).toContainText(en['tokens.organization'])
  await expectAccessible(page)
  await token.getByRole('button', { name: en['tokens.revoke'] }).click()
  await expect(token).toContainText(en['tokens.revoked'])
  await expect(token.getByRole('button', { name: en['tokens.revoke'] })).toHaveCount(0)

  await nav(page, en).getByRole('link', { name: en['nav.audit'] }).click()
  await expect(page.getByRole('heading', { level: 1, name: en['audit.title'] })).toBeVisible()
  for (const action of ['project.apply', 'environment.apply', 'deployment.accepted']) {
    await expect(page.getByRole('cell', { name: action, exact: true }).first()).toBeVisible()
  }
  await expectAccessible(page)

  await signOut(page, en)
  await expect(page.getByRole('heading', { name: en['login.title'] })).toBeVisible()
  // The session is gone: a page that needs one leads to the sign-in.
  await page.goto('/team')
  await expect(page).toHaveURL(/\/login\?redirect=/)
  await expect(page.getByRole('heading', { name: en['login.title'] })).toBeVisible()

  expect(csp).toEqual([])
  expect(errors).toEqual([])
})

test('Persian (RTL): sign in and the project list', async ({ page }) => {
  const { csp, errors } = await open(page, 'fa', 'light')
  await page.goto('/login')
  await expect(page.locator('html')).toHaveAttribute('dir', 'rtl')
  await expect(page.locator('html')).toHaveAttribute('lang', 'fa')
  await expect(page.getByRole('heading', { name: fa['login.title'] })).toBeVisible()
  await expectAccessible(page)

  await signIn(page, fa)
  const project = page.getByRole('link', { name: new RegExp(PROJECT.title) })
  await expect(project).toBeVisible()
  await expect(project).toContainText(fa['projects.environmentsOne'])
  await expect(nav(page, fa).getByRole('link', { name: fa['nav.home'] })).toHaveAttribute(
    'aria-current',
    'page',
  )
  await expectAccessible(page)

  expect(csp).toEqual([])
  expect(errors).toEqual([])
})
