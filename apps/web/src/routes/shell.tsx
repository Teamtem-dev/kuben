import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, Outlet, useRouter } from '@tanstack/react-router'
import { logout } from '../lib/api'
import { useLiveUpdates } from '../lib/live'

const route = getRouteApi('/_authed')

export function AppShell() {
  const { me } = route.useRouteContext()
  const router = useRouter()
  const queryClient = useQueryClient()
  useLiveUpdates()

  const signOut = useMutation({
    mutationFn: logout,
    onSettled: async () => {
      queryClient.clear()
      await router.navigate({ to: '/login', search: {} })
    },
  })

  return (
    <div className="min-h-dvh">
      <header className="sticky top-0 z-10 border-white/10 border-b bg-slate-950/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-6">
          <Link to="/" className="font-semibold tracking-tight">
            kuben
          </Link>
          <nav className="flex gap-4 text-slate-400 text-sm">
            <Link to="/" activeProps={{ className: 'text-slate-100' }} activeOptions={{ exact: true }}>
              Projects
            </Link>
            <Link to="/team" activeProps={{ className: 'text-slate-100' }}>
              Team
            </Link>
            <Link to="/tokens" activeProps={{ className: 'text-slate-100' }}>
              API tokens
            </Link>
            <Link to="/audit" activeProps={{ className: 'text-slate-100' }}>
              Audit
            </Link>
          </nav>
          <div className="ms-auto flex items-center gap-3 text-sm">
            <Link to="/account" className="text-slate-400 hover:text-slate-100">
              {me.display_name ?? me.email}
            </Link>
            <button
              type="button"
              onClick={() => signOut.mutate()}
              disabled={signOut.isPending}
              className="rounded-md border border-white/10 px-2.5 py-1 transition hover:bg-white/5"
            >
              Sign out
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-6 py-8">
        <Outlet />
      </main>
    </div>
  )
}
