import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useEffect, useId, useState } from 'react'
import { AuthLayout, control } from '../components/ui'
import { completeSetup, meQuery, setupQuery } from '../lib/api'
import { usePrefs } from '../lib/prefs'
import { ApiError, problemMessage } from '../lib/problem'
import { setupTokenFrom, tunnelFor } from '../lib/setupToken'

const route = getRouteApi('/setup')

/**
 * First run: create the organization and its admin account. On a public
 * address the request carries the token from the installer's link
 * (`#token=`, taken out of the address bar once read); without one in the
 * link, the form asks for it. Over plain HTTP from another machine the
 * password is never asked for: the page explains the secure ways in.
 */
export function SetupPage() {
  const { token: queryToken } = route.useSearch()
  const { t } = usePrefs()
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
      ? t('setup.expired')
      : error && !insecure
        ? problemMessage(error)
        : null

  if (insecure) {
    const tunnel = tunnelFor(window.location.hostname, window.location.port, token)
    return (
      <AuthLayout>
        <section className="w-full max-w-lg space-y-4 rounded-2xl border border-line bg-surface p-8 shadow-2xl shadow-black/40">
          <h1 className="font-semibold text-2xl tracking-tight">{t('setup.secureTitle')}</h1>
          <p className="text-fg-soft text-sm">{t('setup.secureLead')}</p>
          <ol className="list-decimal space-y-3 ps-5 text-fg-soft text-sm">
            <li>
              {t('setup.viaTunnel')}
              <code className="mt-1 block rounded-lg bg-canvas px-3 py-2 font-mono text-xs" dir="ltr">
                {tunnel.command}
              </code>
              {t('setup.thenOpen')}{' '}
              <code className="rounded bg-canvas px-1 font-mono text-xs" dir="ltr">
                {tunnel.link}
              </code>
            </li>
            <li>
              {t('setup.viaHttps')}{' '}
              <code dir="ltr" className="font-mono text-xs">
                kuben setup --domain apps.example.com --acme-email you@example.com
              </code>{' '}
              {t('setup.onServerOpen')}{' '}
              <code dir="ltr" className="font-mono text-xs">
                https://kuben.apps.example.com
              </code>
              .
            </li>
            <li>
              {t('setup.viaHttp')}{' '}
              <code dir="ltr" className="font-mono text-xs">
                kuben setup --allow-http-setup
              </code>{' '}
              ({t('setup.or')}{' '}
              <code dir="ltr" className="font-mono text-xs">
                security.insecure_setup = true
              </code>
              ).
            </li>
          </ol>
        </section>
      </AuthLayout>
    )
  }

  return (
    <AuthLayout>
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-5 rounded-2xl border border-line bg-surface p-8 shadow-2xl shadow-black/40"
      >
        <header className="space-y-1">
          <h1 className="font-semibold text-2xl tracking-tight">{t('setup.title')}</h1>
          <p className="text-muted text-sm">{t('setup.lead')}</p>
        </header>

        <div className="space-y-1.5">
          <label htmlFor={orgId} className="font-medium text-sm">
            {t('setup.organization')}
          </label>
          <input
            id={orgId}
            name="org_name"
            type="text"
            autoComplete="organization"
            required
            placeholder="ACME"
            className={control}
          />
        </div>

        <div className="space-y-1.5">
          <label htmlFor={emailId} className="font-medium text-sm">
            {t('login.email')}
          </label>
          <input
            id={emailId}
            name="email"
            type="email"
            autoComplete="username"
            required
            className={control}
          />
        </div>

        <div className="space-y-1.5">
          <label htmlFor={passwordId} className="font-medium text-sm">
            {t('login.password')}
          </label>
          <input
            id={passwordId}
            name="password"
            type="password"
            autoComplete="new-password"
            minLength={12}
            required
            className={control}
          />
          <p className="text-subtle text-xs">{t('setup.passwordHint')}</p>
        </div>

        {askForToken && (
          <div className="space-y-1.5">
            <label htmlFor={tokenId} className="font-medium text-sm">
              {t('setup.token')}
            </label>
            <input id={tokenId} name="token" type="text" autoComplete="off" required className={control} />
            <p className="text-subtle text-xs">
              {t('setup.tokenHintBefore')} <code dir="ltr">kuben setup-token</code>{' '}
              {t('setup.tokenHintAfter')}
            </p>
          </div>
        )}

        {message && (
          <p role="alert" className="rounded-lg bg-danger/10 px-3 py-2 text-danger text-sm">
            {message}
          </p>
        )}

        <button
          type="submit"
          disabled={mutation.isPending || status.isPending}
          className="w-full rounded-lg bg-accent px-3 py-2 font-medium text-sm text-on-accent transition hover:bg-accent-hover disabled:opacity-60"
        >
          {mutation.isPending ? t('ui.creating') : t('setup.submit')}
        </button>
      </form>
    </AuthLayout>
  )
}
