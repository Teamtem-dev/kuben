import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Badge, Button, Card, ErrorNote, PageHeader, Select, TextField } from '../components/ui'
import { inviteMember, membersQuery, removeMember, updateMember } from '../lib/api'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'

const route = getRouteApi('/_authed')
const ROLES = ['viewer', 'developer', 'admin', 'owner'] as const

export function TeamPage() {
  const { me } = route.useRouteContext()
  const { t } = usePrefs()
  const { data: members } = useSuspenseQuery(membersQuery)
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['members'] })
  const [invited, setInvited] = useState<{ email: string; password: string } | null>(null)

  const invite = useMutation({
    mutationFn: ({ email, role }: { email: string; role: string }) => inviteMember(email, role),
    onSuccess: async (result) => {
      if (result.temporary_password) {
        setInvited({ email: result.member.email, password: result.temporary_password })
      }
      await refresh()
    },
  })
  const change = useMutation({
    mutationFn: ({ id, role }: { id: string; role: string }) => updateMember(id, role),
    onSettled: refresh,
  })
  const remove = useMutation({ mutationFn: removeMember, onSettled: refresh })

  function onInvite(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    invite.mutate(
      { email: String(form.get('email') ?? '').trim(), role: String(form.get('role') ?? 'developer') },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <section className="space-y-6">
      <PageHeader title={t('nav.team')} subtitle={t('team.lead')} />

      <Card title={t('team.invite')}>
        <form onSubmit={onInvite} className="grid gap-3 sm:grid-cols-[1fr_12rem_auto] sm:items-end">
          <TextField
            label={t('login.email')}
            name="email"
            type="email"
            required
            placeholder="carol@example.com"
          />
          <Select label={t('team.role')} name="role" defaultValue="developer">
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {t(`team.role.${r}`)}
              </option>
            ))}
          </Select>
          <Button type="submit" disabled={invite.isPending}>
            {invite.isPending ? t('team.inviting') : t('team.inviteButton')}
          </Button>
        </form>
        <div className="mt-3 space-y-3">
          <ErrorNote error={invite.error} />
          {invited && (
            <div role="status" className="rounded-lg border border-ok/30 bg-ok/5 p-3 text-sm">
              <p>
                {t('team.tempPasswordFor')} <strong dir="ltr">{invited.email}</strong>{' '}
                {t('team.tempPasswordNote')}
              </p>
              <code
                dir="ltr"
                className="mt-2 block select-all rounded bg-inset px-2 py-1 text-start font-mono"
              >
                {invited.password}
              </code>
            </div>
          )}
        </div>
      </Card>

      <Card title={fill(t('team.members'), { count: members.length })}>
        <ErrorNote error={change.error ?? remove.error} />
        <ul className="divide-y divide-line-soft">
          {members.map((m) => (
            <li key={m.id} className="flex flex-wrap items-center justify-between gap-3 py-3">
              <div className="min-w-0">
                <p className="truncate font-medium text-sm">
                  <span dir="auto">{m.display_name ?? m.email}</span>{' '}
                  {m.id === me.id && <Badge>{t('team.you')}</Badge>}
                </p>
                <p className="truncate text-subtle text-xs">
                  <span dir="ltr">{m.email}</span>
                  {m.must_change_password && ` · ${t('team.invitationPending')}`}
                </p>
              </div>
              <div className="flex items-center gap-2">
                <select
                  aria-label={fill(t('team.roleOf'), { email: m.email })}
                  value={m.role}
                  disabled={m.id === me.id || change.isPending}
                  onChange={(e) => change.mutate({ id: m.id, role: e.target.value })}
                  className="rounded-md border border-line bg-canvas px-2 py-1 text-sm"
                >
                  {ROLES.map((r) => (
                    <option key={r} value={r}>
                      {t(`team.role.${r}`)}
                    </option>
                  ))}
                </select>
                <Button
                  variant="ghost"
                  disabled={m.id === me.id || remove.isPending}
                  onClick={() => remove.mutate(m.id)}
                >
                  {t('ui.remove')}
                </Button>
              </div>
            </li>
          ))}
        </ul>
      </Card>
    </section>
  )
}
