import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useId } from 'react'
import { login, meQuery } from '../lib/api'
import { problemMessage } from '../lib/problem'

const route = getRouteApi('/login')

const inputClass =
  'w-full rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm outline-none transition focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30'

export function LoginPage() {
  const { redirect } = route.useSearch()
  const router = useRouter()
  const queryClient = useQueryClient()
  const emailId = useId()
  const passwordId = useId()

  const mutation = useMutation({
    mutationFn: (creds: { email: string; password: string }) => login(creds.email, creds.password),
    onSuccess: (user) => {
      queryClient.setQueryData(meQuery.queryKey, user)
      router.history.push(redirect ?? '/')
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    mutation.mutate({ email: String(form.get('email') ?? ''), password: String(form.get('password') ?? '') })
  }

  return (
    <main className="grid min-h-dvh place-items-center p-6">
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-5 rounded-2xl border border-white/10 bg-slate-900/60 p-8 shadow-2xl shadow-black/40"
      >
        <header className="space-y-1">
          <h1 className="font-semibold text-2xl tracking-tight">Sign in to Kuben</h1>
          <p className="text-slate-400 text-sm">Enter your email and password.</p>
        </header>

        <div className="space-y-1.5">
          <label htmlFor={emailId} className="font-medium text-sm">
            Email
          </label>
          <input
            id={emailId}
            name="email"
            type="email"
            autoComplete="username"
            required
            className={inputClass}
          />
        </div>

        <div className="space-y-1.5">
          <label htmlFor={passwordId} className="font-medium text-sm">
            Password
          </label>
          <input
            id={passwordId}
            name="password"
            type="password"
            autoComplete="current-password"
            required
            className={inputClass}
          />
        </div>

        {mutation.isError && (
          <p role="alert" className="rounded-lg bg-red-500/10 px-3 py-2 text-red-300 text-sm">
            {problemMessage(mutation.error)}
          </p>
        )}

        <button
          type="submit"
          disabled={mutation.isPending}
          className="w-full rounded-lg bg-sky-500 px-3 py-2 font-medium text-sm text-slate-950 transition hover:bg-sky-400 disabled:opacity-60"
        >
          {mutation.isPending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </main>
  )
}
