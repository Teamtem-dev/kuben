import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, Outlet, useRouter, useRouterState } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { Icon, Logo } from '../components/brand'
import { Preferences } from '../components/preferences'
import { logout } from '../lib/api'
import { useLiveUpdates } from '../lib/live'
import type { MessageKey } from '../lib/messages'
import { usePrefs } from '../lib/prefs'

const route = getRouteApi('/_authed')

const NAV = [
  { to: '/', label: 'nav.projects', icon: 'projects', exact: true },
  { to: '/team', label: 'nav.team', icon: 'team', exact: false },
  { to: '/tokens', label: 'nav.tokens', icon: 'tokens', exact: false },
  { to: '/incidents', label: 'nav.incidents', icon: 'incidents', exact: false },
  { to: '/webhooks', label: 'nav.webhooks', icon: 'webhooks', exact: false },
  { to: '/domains', label: 'nav.domains', icon: 'domains', exact: false },
  { to: '/audit', label: 'nav.audit', icon: 'audit', exact: false },
] as const satisfies readonly {
  to: string
  label: MessageKey
  icon: Parameters<typeof Icon>[0]['name']
  exact: boolean
}[]

function Navigation() {
  const { t } = usePrefs()
  return (
    <nav aria-label={t('nav.label')} className="space-y-1">
      {NAV.map((item) => (
        <Link
          key={item.to}
          to={item.to}
          activeOptions={{ exact: item.exact }}
          className="flex items-center gap-3 rounded-lg px-3 py-2 text-muted text-sm transition hover:bg-hover hover:text-fg"
          activeProps={{ className: 'bg-hover text-fg', 'aria-current': 'page' }}
        >
          <Icon name={item.icon} />
          {t(item.label)}
        </Link>
      ))}
    </nav>
  )
}

export function AppShell() {
  const { me } = route.useRouteContext()
  const router = useRouter()
  const queryClient = useQueryClient()
  const { t } = usePrefs()
  const [menuOpen, setMenuOpen] = useState(false)
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  useLiveUpdates()

  // A page opened from the menu closes it.
  useEffect(() => setMenuOpen(false), [pathname])

  const signOut = useMutation({
    mutationFn: logout,
    onSettled: async () => {
      queryClient.clear()
      await router.navigate({ to: '/login', search: {} })
    },
  })

  return (
    <div className="min-h-dvh md:grid md:grid-cols-[15rem_1fr]">
      <a
        href="#content"
        className="sr-only focus:not-sr-only focus:absolute focus:start-4 focus:top-4 focus:z-30 focus:rounded-md focus:bg-accent focus:px-3 focus:py-2 focus:text-on-accent"
      >
        {t('shell.skip')}
      </a>
      <aside
        className={`${menuOpen ? 'fixed inset-y-0 start-0 z-20 flex w-64' : 'hidden'} flex-col gap-6 border-line border-e bg-canvas p-4 md:sticky md:top-0 md:flex md:h-dvh md:w-auto`}
      >
        <div className="flex items-center justify-between">
          <Link to="/" className="rounded-md">
            <Logo label={t('app.name')} />
          </Link>
          <button
            type="button"
            onClick={() => setMenuOpen(false)}
            className="rounded-md p-1.5 text-muted hover:bg-hover md:hidden"
            aria-label={t('shell.closeMenu')}
          >
            <Icon name="close" />
          </button>
        </div>
        <Navigation />
      </aside>
      {menuOpen && (
        <button
          type="button"
          aria-label={t('shell.closeMenu')}
          onClick={() => setMenuOpen(false)}
          className="fixed inset-0 z-10 bg-inset md:hidden"
        />
      )}
      <div className="min-w-0">
        <header className="sticky top-0 z-10 border-line border-b bg-canvas/80 backdrop-blur">
          <div className="flex h-14 items-center gap-3 px-4 md:px-6">
            <button
              type="button"
              onClick={() => setMenuOpen(true)}
              className="rounded-md p-1.5 text-muted hover:bg-hover md:hidden"
              aria-label={t('shell.menu')}
              aria-expanded={menuOpen}
            >
              <Icon name="menu" />
            </button>
            <div className="ms-auto flex items-center gap-3 text-sm">
              <Preferences />
              <Link
                to="/account"
                className="max-w-48 truncate text-muted hover:text-fg"
                title={t('shell.account')}
              >
                {me.display_name ?? me.email}
              </Link>
              <button
                type="button"
                onClick={() => signOut.mutate()}
                disabled={signOut.isPending}
                className="rounded-md border border-line px-2.5 py-1 transition hover:bg-hover"
              >
                {t('shell.signOut')}
              </button>
            </div>
          </div>
        </header>
        <main id="content" tabIndex={-1} className="mx-auto max-w-6xl px-4 py-8 outline-none md:px-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
