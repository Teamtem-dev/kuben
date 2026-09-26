import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useEffect, useState } from 'react'
import { AuthShell, ErrorAlert, TextInput } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Spinner } from '@/components/ui/spinner'
import { completeSetup, meQuery, setupQuery } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'
import { setupTokenFrom, tunnelFor } from '@/lib/setupToken'

const route = getRouteApi('/setup')

const code = 'rounded bg-muted px-1 py-0.5 font-mono text-xs'

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
  const failure =
    error instanceof ApiError && error.status === 403 && !insecure
      ? new Error(t('setup.expired'))
      : error && !insecure
        ? error
        : null

  if (insecure) {
    const tunnel = tunnelFor(window.location.hostname, window.location.port, token)
    return (
      <AuthShell>
        <Card className="w-full max-w-lg shadow-lg">
          <CardHeader>
            <CardTitle>
              <h1 className="text-2xl tracking-tight">{t('setup.secureTitle')}</h1>
            </CardTitle>
            <CardDescription>{t('setup.secureLead')}</CardDescription>
          </CardHeader>
          <CardContent>
            <ol className="list-decimal space-y-3 ps-5 text-sm">
              <li>
                {t('setup.viaTunnel')}
                <code className="mt-1 block rounded-md bg-muted px-3 py-2 font-mono text-xs" dir="ltr">
                  {tunnel.command}
                </code>
                {t('setup.thenOpen')}{' '}
                <code className={code} dir="ltr">
                  {tunnel.link}
                </code>
              </li>
              <li>
                {t('setup.viaHttps')}{' '}
                <code dir="ltr" className={code}>
                  kuben setup --domain apps.example.com --acme-email you@example.com
                </code>{' '}
                {t('setup.onServerOpen')}{' '}
                <code dir="ltr" className={code}>
                  https://kuben.apps.example.com
                </code>
                .
              </li>
              <li>
                {t('setup.viaHttp')}{' '}
                <code dir="ltr" className={code}>
                  kuben setup --allow-http-setup
                </code>{' '}
                ({t('setup.or')}{' '}
                <code dir="ltr" className={code}>
                  security.insecure_setup = true
                </code>
                ).
              </li>
            </ol>
          </CardContent>
        </Card>
      </AuthShell>
    )
  }

  return (
    <AuthShell>
      <Card className="w-full max-w-sm shadow-lg">
        <CardHeader>
          <CardTitle>
            <h1 className="text-2xl tracking-tight">{t('setup.title')}</h1>
          </CardTitle>
          <CardDescription>{t('setup.lead')}</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-5">
            <TextInput
              label={t('setup.organization')}
              name="org_name"
              type="text"
              autoComplete="organization"
              required
              placeholder="ACME"
            />
            <TextInput
              label={t('login.email')}
              name="email"
              type="email"
              autoComplete="username"
              dir="ltr"
              required
            />
            <TextInput
              label={t('login.password')}
              name="password"
              type="password"
              autoComplete="new-password"
              dir="ltr"
              minLength={12}
              required
              hint={t('setup.passwordHint')}
            />
            {askForToken && (
              <TextInput
                label={t('setup.token')}
                name="token"
                type="text"
                autoComplete="off"
                dir="ltr"
                required
                hint={
                  <>
                    {t('setup.tokenHintBefore')} <code dir="ltr">kuben setup-token</code>{' '}
                    {t('setup.tokenHintAfter')}
                  </>
                }
              />
            )}

            <ErrorAlert error={failure} />

            <Button type="submit" className="w-full" disabled={mutation.isPending || status.isPending}>
              {mutation.isPending && <Spinner role="presentation" aria-hidden="true" />}
              {mutation.isPending ? t('ui.creating') : t('setup.submit')}
            </Button>
          </form>
        </CardContent>
      </Card>
    </AuthShell>
  )
}
