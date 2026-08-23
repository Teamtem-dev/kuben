import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Card, ErrorNote, PageHeader, TextField } from '../components/ui'
import { changePassword, meQuery } from '../lib/api'

const route = getRouteApi('/_authed')
const MIN_LENGTH = 12

export function AccountPage() {
  const { me } = route.useRouteContext()
  const queryClient = useQueryClient()
  const router = useRouter()
  const [mismatch, setMismatch] = useState(false)
  const [done, setDone] = useState(false)

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
    if (next !== repeat) return
    save.mutate(
      { current: String(form.get('current') ?? ''), next },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <section className="max-w-xl space-y-6">
      <PageHeader title="Account" subtitle={me.email} />
      {me.must_change_password && (
        <p role="alert" className="rounded-lg bg-amber-500/10 px-3 py-2 text-amber-200 text-sm">
          You signed in with a temporary password. Choose your own password to continue.
        </p>
      )}
      <Card title="Change password">
        <form onSubmit={onSubmit} className="space-y-3">
          <TextField
            label="Current password"
            name="current"
            type="password"
            autoComplete="current-password"
            required
          />
          <TextField
            label="New password"
            name="next"
            type="password"
            autoComplete="new-password"
            minLength={MIN_LENGTH}
            required
            hint={`At least ${MIN_LENGTH} characters. Other sessions are signed out.`}
          />
          <TextField
            label="Repeat new password"
            name="repeat"
            type="password"
            autoComplete="new-password"
            required
          />
          <div className="flex items-center gap-3">
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Change password'}
            </Button>
            {mismatch && <ErrorNote error={new Error('The new passwords do not match.')} />}
            <ErrorNote error={save.error} />
            {done && !save.isPending && <span className="text-emerald-300 text-sm">Password changed.</span>}
          </div>
        </form>
      </Card>
    </section>
  )
}
