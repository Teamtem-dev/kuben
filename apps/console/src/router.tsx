import type { QueryClient } from '@tanstack/react-query'
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  type ErrorComponentProps,
  Link,
  redirect,
} from '@tanstack/react-router'
import {
  appQuery,
  appsQuery,
  environmentQuery,
  environmentsQuery,
  membersQuery,
  meQuery,
  projectQuery,
  projectsQuery,
  setupQuery,
  tokensQuery,
} from './lib/api'
import { problemMessage } from './lib/problem'
import { AccountPage } from './routes/account'
import { AppPage } from './routes/app'
import { AuditPage } from './routes/audit'
import { EnvironmentPage } from './routes/environment'
import { LoginPage } from './routes/login'
import { ProjectPage } from './routes/project'
import { ProjectsPage } from './routes/projects'
import { SetupPage } from './routes/setup'
import { AppShell } from './routes/shell'
import { TeamPage } from './routes/team'
import { TokensPage } from './routes/tokens'

interface RouterContext {
  queryClient: QueryClient
}

/** Only same-origin absolute paths may be used as a post-login target. */
export function safeRedirect(value: unknown): string | undefined {
  return typeof value === 'string' && value.startsWith('/') && !value.startsWith('//') ? value : undefined
}

function RouteError({ error }: ErrorComponentProps) {
  return (
    <div role="alert" className="space-y-3 rounded-xl border border-red-500/30 bg-red-500/5 p-6">
      <p className="font-medium">{problemMessage(error)}</p>
      <Link to="/" className="text-sky-300 text-sm hover:underline">
        Back to projects
      </Link>
    </div>
  )
}

function Pending() {
  return <p className="text-slate-500 text-sm">Loading…</p>
}

const rootRoute = createRootRouteWithContext<RouterContext>()()

/** Until the first admin exists every page leads to `/setup`; afterwards `/setup` leads to login. */
async function setupNeeded(queryClient: QueryClient): Promise<boolean> {
  const status = await queryClient.ensureQueryData(setupQuery)
  return status.needed
}

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/setup',
  validateSearch: (search: Record<string, unknown>): { token?: string } => ({
    token: typeof search.token === 'string' && search.token !== '' ? search.token : undefined,
  }),
  beforeLoad: async ({ context }) => {
    if (!(await setupNeeded(context.queryClient))) throw redirect({ to: '/login' })
  },
  component: SetupPage,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: safeRedirect(search.redirect),
  }),
  beforeLoad: async ({ context }) => {
    if (await setupNeeded(context.queryClient)) throw redirect({ to: '/setup' })
  },
  component: LoginPage,
})

/** Pathless layout: everything below it requires a session. */
const authedRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: '_authed',
  beforeLoad: async ({ context, location }) => {
    const me = await context.queryClient.ensureQueryData(meQuery)
    if (!me) {
      if (await setupNeeded(context.queryClient)) throw redirect({ to: '/setup' })
      throw redirect({ to: '/login', search: { redirect: location.href } })
    }
    // Invited users must replace their temporary password first (the API
    // enforces the same rule and answers 403 everywhere else).
    if (me.must_change_password && location.pathname !== '/account') throw redirect({ to: '/account' })
    return { me }
  },
  component: AppShell,
})

const projectsRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/',
  loader: ({ context }) => context.queryClient.ensureQueryData(projectsQuery),
  component: ProjectsPage,
})

const projectRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project',
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(projectQuery(params.project)),
      context.queryClient.ensureQueryData(environmentsQuery(params.project)),
    ]),
  component: ProjectPage,
})

const environmentRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project/$environment',
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(projectQuery(params.project)),
      context.queryClient.ensureQueryData(environmentQuery(params.project, params.environment)),
      context.queryClient.ensureQueryData(appsQuery(params.project, params.environment)),
    ]),
  component: EnvironmentPage,
})

const appRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project/$environment/$app',
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(appQuery(params.project, params.environment, params.app)),
  component: AppPage,
})

const teamRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/team',
  loader: ({ context }) => context.queryClient.ensureQueryData(membersQuery),
  component: TeamPage,
})

const tokensRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/tokens',
  loader: ({ context }) => context.queryClient.ensureQueryData(tokensQuery),
  component: TokensPage,
})

const auditRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/audit',
  component: AuditPage,
})

const accountRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/account',
  component: AccountPage,
})

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  authedRoute.addChildren([
    projectsRoute,
    projectRoute,
    environmentRoute,
    appRoute,
    teamRoute,
    tokensRoute,
    auditRoute,
    accountRoute,
  ]),
])

export function createAppRouter(queryClient: QueryClient) {
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultPreload: 'intent',
    defaultErrorComponent: RouteError,
    defaultPendingComponent: Pending,
    scrollRestoration: true,
  })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
