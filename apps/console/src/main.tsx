import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { meQuery } from './lib/api'
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

const root = document.getElementById('root')
if (!root) throw new Error('#root is missing from index.html')

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
