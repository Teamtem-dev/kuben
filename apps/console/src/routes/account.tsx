import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, useRouter } from '@tanstack/react-router'
import { KeyRoundIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import { ErrorAlert, Notice, PageHeader, Section, TextInput } from '@/components/kit'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Button } from '@/components/ui/button'
import { changePassword, meQuery } from '@/lib/api'
import { initials } from '@/lib/initials'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'

const route = getRouteApi('/_authed')
const MIN_LENGTH = 12

export function AccountPage() {
  const { me } = route.useRouteContext()
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const router = useRouter()
  const [mismatch, setMismatch] = useState(false)
  const [done, setDone] = useState(false)
  const name = me.display_name ?? me.email

  const save = useMutation({
    mutationFn: ({ current, next }: { current: string; next: string }) => changePassword(current, next),
    onSuccess: async () => {
      setDone(true)
      // `ensureQueryData` in the route guard would keep serving the cached
      // user, so clear the flag in the cache before re-running the guards.
      queryClient.setQueryData(meQuery.queryKey, (m) => (m ? { ...m, must_change_password: false } : m))
      await router.invalidate()
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const next = String(form.get('next') ?? '')
    const repeat = String(form.get('repeat') ?? '')
    setMismatch(next !== repeat)
    setDone(false)
    if (next !== repeat) return
    save.mutate(
      { current: String(form.get('current') ?? ''), next },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <div className="max-w-2xl space-y-6">
      <PageHeader title={t('shell.account')} />

      {me.must_change_password && (
        <Notice tone="warning" role="alert">
          {t('account.temporary')}
        </Notice>
      )}

      <Section title={t('account.profile')}>
        <div className="flex items-center gap-4">
          <Avatar className="size-12">
            <AvatarFallback>{initials(name)}</AvatarFallback>
          </Avatar>
          <div className="min-w-0">
            <p dir="auto" className="truncate font-medium">
              {name}
            </p>
            <p dir="ltr" className="truncate text-start text-muted-foreground text-sm">
              {me.email}
            </p>
          </div>
        </div>
      </Section>

      <Section title={t('account.changePassword')}>
        <form onSubmit={onSubmit} className="grid gap-4">
          <TextInput
            label={t('account.current')}
            name="current"
            type="password"
            autoComplete="current-password"
            dir="ltr"
            required
          />
          <TextInput
            label={t('account.new')}
            name="next"
            type="password"
            autoComplete="new-password"
            dir="ltr"
            minLength={MIN_LENGTH}
            required
            hint={fill(t('account.newHint'), { count: MIN_LENGTH })}
          />
          <TextInput
            label={t('account.repeat')}
            name="repeat"
            type="password"
            autoComplete="new-password"
            dir="ltr"
            required
          />
          {mismatch && <ErrorAlert error={new Error(t('account.mismatch'))} />}
          <ErrorAlert error={save.error} />
          {done && !save.isPending && <Notice tone="success">{t('account.changed')}</Notice>}
          <div>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? t('ui.saving') : t('account.changePassword')}
            </Button>
          </div>
        </form>
      </Section>

      {!me.must_change_password && (
        <Section
          title={t('nav.tokens')}
          description={t('tokens.lead')}
          actions={
            <Button variant="outline" asChild>
              <Link to="/tokens">
                <KeyRoundIcon aria-hidden="true" />
                {t('account.manageTokens')}
              </Link>
            </Button>
          }
        />
      )}
    </div>
  )
}
