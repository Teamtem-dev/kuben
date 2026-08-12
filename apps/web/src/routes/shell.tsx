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

