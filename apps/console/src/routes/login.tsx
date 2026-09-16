import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useId } from 'react'
import { AuthLayout, control } from '../components/ui'
import { login, meQuery } from '../lib/api'
import { usePrefs } from '../lib/prefs'
import { problemMessage } from '../lib/problem'

const route = getRouteApi('/login')

export function LoginPage() {
  const { redirect } = route.useSearch()
  const router = useRouter()
  const queryClient = useQueryClient()
  const emailId = useId()
  const passwordId = useId()
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
    <AuthLayout>
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-5 rounded-2xl border border-line bg-surface p-8 shadow-2xl shadow-black/40"
      >
        <header className="space-y-1">
          <h1 className="font-semibold text-2xl tracking-tight">{t('login.title')}</h1>
          <p className="text-muted text-sm">{t('login.lead')}</p>
        </header>

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
            autoComplete="current-password"
            required
            className={control}
          />
        </div>

        {mutation.isError && (
          <p role="alert" className="rounded-lg bg-danger/10 px-3 py-2 text-danger text-sm">
            {problemMessage(mutation.error)}
          </p>
        )}

        <button
          type="submit"
          disabled={mutation.isPending}
          className="w-full rounded-lg bg-accent px-3 py-2 font-medium text-sm text-on-accent transition hover:bg-accent-hover disabled:opacity-60"
        >
          {mutation.isPending ? t('login.submitting') : t('login.submit')}
        </button>
      </form>
    </AuthLayout>
  )
}
