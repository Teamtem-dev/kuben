import type { QueryClient } from '@tanstack/react-query'
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  type ErrorComponentProps,
  Link,
  lazyRouteComponent,
  redirect,
} from '@tanstack/react-router'
import { Skeleton } from './components/ui/skeleton'
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
import { type AppTab, appTabFrom } from './lib/app-tabs'
import { usePrefs } from './lib/prefs'
import { problemMessage } from './lib/problem'
import { AppShell } from './routes/shell'

/**
 * Pages load on demand (each its own chunk, preloaded on hover or focus of a
 * link); the shell, the router and the data layer are in the entry chunk.
 */
const page = {
  AccountPage: lazyRouteComponent(() => import('./routes/account'), 'AccountPage'),
  AppPage: lazyRouteComponent(() => import('./routes/app'), 'AppPage'),
  AuditPage: lazyRouteComponent(() => import('./routes/audit'), 'AuditPage'),
  DoctorPage: lazyRouteComponent(() => import('./routes/doctor'), 'DoctorPage'),
  DomainsPage: lazyRouteComponent(() => import('./routes/domains'), 'DomainsPage'),
  EnvironmentPage: lazyRouteComponent(() => import('./routes/environment'), 'EnvironmentPage'),
  IncidentsPage: lazyRouteComponent(() => import('./routes/incidents'), 'IncidentsPage'),
  LoginPage: lazyRouteComponent(() => import('./routes/login'), 'LoginPage'),
  ProjectPage: lazyRouteComponent(() => import('./routes/project'), 'ProjectPage'),
  ProjectsPage: lazyRouteComponent(() => import('./routes/projects'), 'ProjectsPage'),
  SetupPage: lazyRouteComponent(() => import('./routes/setup'), 'SetupPage'),
  StatusPage: lazyRouteComponent(() => import('./routes/status'), 'StatusPage'),
  TeamPage: lazyRouteComponent(() => import('./routes/team'), 'TeamPage'),
  TokensPage: lazyRouteComponent(() => import('./routes/tokens'), 'TokensPage'),
  WebhooksPage: lazyRouteComponent(() => import('./routes/webhooks'), 'WebhooksPage'),
}

interface RouterContext {
  queryClient: QueryClient
}

/** Only same-origin absolute paths may be used as a post-login target. */
export function safeRedirect(value: unknown): string | undefined {
  return typeof value === 'string' && value.startsWith('/') && !value.startsWith('//') ? value : undefined
}

function RouteError({ error }: ErrorComponentProps) {
  const { t } = usePrefs()
  return (
    <div role="alert" className="space-y-3 rounded-xl border border-destructive/30 bg-destructive/5 p-6">
      <p className="font-medium">{problemMessage(error)}</p>
      <Link to="/" className="text-link text-sm hover:underline">
        {t('common.backToProjects')}
      </Link>
    </div>
  )
}

/** A page loading: its shape in grey (kept in the entry chunk, no kit needed). */
function Pending() {
  const { t } = usePrefs()
  return (
    <div role="status" className="space-y-6">
      <span className="sr-only">{t('common.loading')}</span>
      <div aria-hidden="true" className="space-y-2">
        <Skeleton className="h-8 w-56" />
        <Skeleton className="h-4 w-80 max-w-full" />
      </div>
      <div aria-hidden="true" className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-24 rounded-xl" />
        ))}
      </div>
    </div>
  )
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
  component: page.SetupPage,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  validateSearch: (search: Record<string, unknown>): { redirect?: string; error?: 'sso' } => ({
    redirect: safeRedirect(search.redirect),
    error: search.error === 'sso' ? 'sso' : undefined,
  }),
  beforeLoad: async ({ context }) => {
    if (await setupNeeded(context.queryClient)) throw redirect({ to: '/setup' })
  },
  component: page.LoginPage,
})

/** A project's public status page: no session needed (M5.3). */
const statusRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/status/$slug',
  component: page.StatusPage,
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
  component: page.ProjectsPage,
})

const projectRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project',
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(projectQuery(params.project)),
      context.queryClient.ensureQueryData(environmentsQuery(params.project)),
    ]),
  component: page.ProjectPage,
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
  component: page.EnvironmentPage,
})

const appRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project/$environment/$app',
  // The open tab; the overview has none, so the plain app URL stays as it was.
  validateSearch: (search: Record<string, unknown>): { tab?: Exclude<AppTab, 'overview'> } => ({
    tab: appTabFrom(search.tab),
  }),
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(appQuery(params.project, params.environment, params.app)),
  component: page.AppPage,
})

const doctorRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/projects/$project/$environment/$app/doctor',
  component: page.DoctorPage,
})

const teamRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/team',
  loader: ({ context }) => context.queryClient.ensureQueryData(membersQuery),
  component: page.TeamPage,
})

const tokensRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/tokens',
  loader: ({ context }) => context.queryClient.ensureQueryData(tokensQuery),
  component: page.TokensPage,
})

const auditRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/audit',
  component: page.AuditPage,
})

const incidentsRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/incidents',
  component: page.IncidentsPage,
})

const webhooksRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/webhooks',
  component: page.WebhooksPage,
})

const domainsRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/domains',
  component: page.DomainsPage,
})

const accountRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: '/account',
  component: page.AccountPage,
})

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  statusRoute,
  authedRoute.addChildren([
    projectsRoute,
    projectRoute,
    environmentRoute,
    appRoute,
    doctorRoute,
    teamRoute,
    tokensRoute,
    auditRoute,
    incidentsRoute,
    webhooksRoute,
    domainsRoute,
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
