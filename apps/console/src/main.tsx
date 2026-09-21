import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { type ReactNode, StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { DirectionProvider } from './components/ui/direction'
import { Toaster } from './components/ui/sonner'
import { TooltipProvider } from './components/ui/tooltip'
import { meQuery } from './lib/api'
import { applyPrefs, direction, PrefsProvider, readPrefs, usePrefs } from './lib/prefs'
import { ApiError } from './lib/problem'
import { createAppRouter } from './router'
import './styles.css'

const queryClient = new QueryClient({
  // A 401 from any query means the session expired: drop it and go to login.
  queryCache: new QueryCache({
    onError: (error) => {
      if (error instanceof ApiError && error.status === 401) {
        queryClient.setQueryData(meQuery.queryKey, null)
        void router.navigate({ to: '/login', search: { redirect: window.location.pathname } })
      }
    },
  }),
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: (failures, error) => !(error instanceof ApiError && error.status < 500) && failures < 2,
    },
  },
})

const router = createAppRouter(queryClient)

// Before the first paint: no flash of the wrong theme or direction.
applyPrefs(readPrefs())

/** Reading direction for Radix, tooltips, and the one toast region, from the preferences. */
function Providers({ children }: { children: ReactNode }) {
  const { locale, theme, t } = usePrefs()
  const dir = direction(locale)
  return (
    <DirectionProvider dir={dir}>
      <TooltipProvider>
        {children}
        <Toaster theme={theme} dir={dir} containerAriaLabel={t('shell.notifications')} />
      </TooltipProvider>
    </DirectionProvider>
  )
}

const root = document.getElementById('root')
if (!root) throw new Error('#root is missing from index.html')

createRoot(root).render(
  <StrictMode>
    <PrefsProvider>
      <Providers>
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
        </QueryClientProvider>
      </Providers>
    </PrefsProvider>
  </StrictMode>,
)
