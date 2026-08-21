import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Badge, Button, Card, ErrorNote, PageHeader, Select, TextField } from '../components/ui'
import { inviteMember, membersQuery, removeMember, updateMember } from '../lib/api'

const route = getRouteApi('/_authed')
const ROLES = ['viewer', 'developer', 'admin', 'owner'] as const

export function TeamPage() {
  const { me } = route.useRouteContext()
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
      <PageHeader
        title="Team"
        subtitle="Roles are hierarchical: viewer < developer < admin < owner. Nobody can grant a role above their own."
      />

      <Card title="Invite a member">
        <form onSubmit={onInvite} className="grid gap-3 sm:grid-cols-[1fr_12rem_auto] sm:items-end">
          <TextField label="Email" name="email" type="email" required placeholder="carol@example.com" />
          <Select label="Role" name="role" defaultValue="developer">
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </Select>
          <Button type="submit" disabled={invite.isPending}>
            {invite.isPending ? 'Inviting…' : 'Invite'}
          </Button>
        </form>
        <div className="mt-3 space-y-3">
          <ErrorNote error={invite.error} />
          {invited && (
            <div
              role="status"
              className="rounded-lg border border-emerald-400/30 bg-emerald-400/5 p-3 text-sm"
            >
              <p>
                Temporary password for <strong>{invited.email}</strong> — shown once. They must replace it at
                first sign-in.
              </p>
              <code className="mt-2 block select-all rounded bg-black/40 px-2 py-1 font-mono">
                {invited.password}
              </code>
            </div>
          )}
        </div>
      </Card>

      <Card title={`Members (${members.length})`}>
        <ErrorNote error={change.error ?? remove.error} />
        <ul className="divide-y divide-white/5">
          {members.map((m) => (
            <li key={m.id} className="flex flex-wrap items-center justify-between gap-3 py-3">
              <div className="min-w-0">
                <p className="truncate font-medium text-sm">
                  {m.display_name ?? m.email} {m.id === me.id && <Badge>you</Badge>}
                </p>
                <p className="truncate text-slate-500 text-xs">
                  {m.email}
                  {m.must_change_password && ' · invitation pending'}
                </p>
              </div>
              <div className="flex items-center gap-2">
                <select
                  aria-label={`Role of ${m.email}`}
                  value={m.role}
                  disabled={m.id === me.id || change.isPending}
                  onChange={(e) => change.mutate({ id: m.id, role: e.target.value })}
                  className="rounded-md border border-white/10 bg-slate-950 px-2 py-1 text-sm"
                >
                  {ROLES.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
                <Button
                  variant="ghost"
