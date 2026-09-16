import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useEffect, useId, useState } from 'react'
import { completeSetup, meQuery, setupQuery } from '../lib/api'
import { ApiError, problemMessage } from '../lib/problem'
import { setupTokenFrom, tunnelFor } from '../lib/setupToken'

const route = getRouteApi('/setup')

const inputClass =
  'w-full rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm outline-none transition focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30'

/**
 * First run: create the organization and its admin account. On a public
 * address the request carries the token from the installer's link
 * (`#token=`, taken out of the address bar once read); without one in the
 * link, the form asks for it. Over plain HTTP from another machine the
 * password is never asked for: the page explains the secure ways in.
 */
export function SetupPage() {
  const { token: queryToken } = route.useSearch()
  const [token] = useState(() => setupTokenFrom(window.location.hash, queryToken))
  useEffect(() => {
    if (window.location.hash || window.location.search) window.history.replaceState(null, '', '/setup')
  }, [])
  const router = useRouter()
  const queryClient = useQueryClient()
  const status = useQuery(setupQuery)
  const orgId = useId()
  const emailId = useId()
  const passwordId = useId()
  const tokenId = useId()

  const mutation = useMutation({
    mutationFn: completeSetup,
    onSuccess: (user) => {
      queryClient.setQueryData(meQuery.queryKey, user)
      queryClient.setQueryData(setupQuery.queryKey, { needed: false, token_required: false, secure: true })
      router.history.push('/')
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const typed = String(form.get('token') ?? '').trim()
    mutation.mutate({
      org_name: String(form.get('org_name') ?? ''),
      email: String(form.get('email') ?? ''),
      password: String(form.get('password') ?? ''),
      token: token ?? (typed || undefined),
    })
  }

  const askForToken = status.data?.token_required === true && !token
  const error = mutation.error
  const insecure =
    status.data?.secure === false || (error instanceof ApiError && error.code === 'insecure_transport')
  const message =
    error instanceof ApiError && error.status === 403 && !insecure
      ? 'The setup link has expired or its token is wrong. Print a new one on the server with `kuben setup-token`.'
      : error && !insecure
        ? problemMessage(error)
        : null

  if (insecure) {
    const tunnel = tunnelFor(window.location.hostname, window.location.port, token)
    return (
      <main className="grid min-h-dvh place-items-center p-6">
        <section className="w-full max-w-lg space-y-4 rounded-2xl border border-white/10 bg-slate-900/60 p-8 shadow-2xl shadow-black/40">
          <h1 className="font-semibold text-2xl tracking-tight">Finish the setup securely</h1>
          <p className="text-slate-300 text-sm">
            This page is served over plain HTTP, so the admin password is not asked for here. Use one of
            these:
          </p>
          <ol className="list-decimal space-y-3 ps-5 text-slate-300 text-sm">
            <li>
              Through an SSH tunnel from your computer:
              <code className="mt-1 block rounded-lg bg-slate-950 px-3 py-2 font-mono text-xs">
                {tunnel.command}
              </code>
              then open{' '}
              <code className="rounded bg-slate-950 px-1 font-mono text-xs" dir="ltr">
                {tunnel.link}
              </code>
            </li>
            <li>
              Over HTTPS: run{' '}
              <code className="font-mono text-xs">
                kuben setup --domain apps.example.com --acme-email you@example.com
              </code>{' '}
              on the server and open <code className="font-mono text-xs">https://kuben.apps.example.com</code>
              .
            </li>
            <li>
              On a network you trust, allow plain HTTP with{' '}
              <code className="font-mono text-xs">security.insecure_setup = true</code>.
            </li>
          </ol>
        </section>
      </main>
    )
  }

  return (
    <main className="grid min-h-dvh place-items-center p-6">
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-5 rounded-2xl border border-white/10 bg-slate-900/60 p-8 shadow-2xl shadow-black/40"
      >
        <header className="space-y-1">
          <h1 className="font-semibold text-2xl tracking-tight">Set up Kuben</h1>
          <p className="text-slate-400 text-sm">
            Create the admin account. You can invite the rest of the team afterwards.
          </p>
        </header>

        <div className="space-y-1.5">
          <label htmlFor={orgId} className="font-medium text-sm">
            Organization
          </label>
          <input
            id={orgId}
            name="org_name"
            type="text"
            autoComplete="organization"
            required
            placeholder="ACME"
            className={inputClass}
          />
        </div>

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
            autoComplete="new-password"
            minLength={12}
            required
            className={inputClass}
          />
          <p className="text-slate-500 text-xs">At least 12 characters.</p>
        </div>

        {askForToken && (
          <div className="space-y-1.5">
            <label htmlFor={tokenId} className="font-medium text-sm">
              Setup token
            </label>
            <input id={tokenId} name="token" type="text" autoComplete="off" required className={inputClass} />
            <p className="text-slate-500 text-xs">
              Printed by the installer; on the server, <code>kuben setup-token</code> prints a new one.
            </p>
          </div>
        )}

        {message && (
          <p role="alert" className="rounded-lg bg-red-500/10 px-3 py-2 text-red-300 text-sm">
            {message}
          </p>
        )}

        <button
          type="submit"
          disabled={mutation.isPending || status.isPending}
          className="w-full rounded-lg bg-sky-500 px-3 py-2 font-medium text-sm text-slate-950 transition hover:bg-sky-400 disabled:opacity-60"
        >
          {mutation.isPending ? 'Creating…' : 'Create admin account'}
        </button>
      </form>
    </main>
  )
}
