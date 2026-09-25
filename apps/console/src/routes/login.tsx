import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import type { FormEvent } from 'react'
import { AuthShell, ErrorAlert, TextInput } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Spinner } from '@/components/ui/spinner'
import { login, meQuery, ssoQuery } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'

const route = getRouteApi('/login')

export function LoginPage() {
  const { redirect, error } = route.useSearch()
  const sso = useQuery(ssoQuery)
  const router = useRouter()
  const queryClient = useQueryClient()
  const { t } = usePrefs()

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
    <AuthShell>
      <Card className="w-full max-w-sm shadow-lg">
        <CardHeader>
          <CardTitle>
            <h1 className="text-2xl tracking-tight">{t('login.title')}</h1>
          </CardTitle>
          <CardDescription>{t('login.lead')}</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-5">
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
              autoComplete="current-password"
              dir="ltr"
              required
            />

            {error === 'sso' && !mutation.isError && <ErrorAlert error={new Error(t('login.ssoFailed'))} />}
            <ErrorAlert error={mutation.error} />

            <Button type="submit" className="w-full" disabled={mutation.isPending}>
              {mutation.isPending && <Spinner role="presentation" aria-hidden="true" />}
              {mutation.isPending ? t('login.submitting') : t('login.submit')}
            </Button>

            {sso.data?.enabled && sso.data.startUrl && (
              <>
                <div className="flex items-center gap-3 text-muted-foreground text-xs">
                  <span className="h-px flex-1 bg-border" />
                  {t('login.or')}
                  <span className="h-px flex-1 bg-border" />
                </div>
                <Button variant="outline" className="w-full" asChild>
                  <a href={`${sso.data.startUrl}?returnTo=${encodeURIComponent(redirect ?? '/')}`}>
                    {t('login.sso')} <span dir="auto">{sso.data.displayName}</span>
                  </a>
                </Button>
              </>
            )}
          </form>
        </CardContent>
      </Card>
    </AuthShell>
  )
}
