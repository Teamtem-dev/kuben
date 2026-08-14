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
