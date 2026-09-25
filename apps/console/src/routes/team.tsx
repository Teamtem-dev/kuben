import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { UserPlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import {
  Copyable,
  ErrorAlert,
  FormDialog,
  Notice,
  PageHeader,
  Section,
  SelectInput,
  Tag,
  TextInput,
} from '@/components/kit'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { NativeSelect } from '@/components/ui/native-select'
import { inviteMember, type Member, membersQuery, removeMember, updateMember } from '@/lib/api'
import { initials } from '@/lib/initials'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'

const route = getRouteApi('/_authed')
const ROLES = ['viewer', 'developer', 'admin', 'owner'] as const

function InviteForm({
  onInvited,
}: {
  onInvited: (invited: { email: string; password: string } | null) => void
}) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const invite = useMutation({
    mutationFn: ({ email, role }: { email: string; role: string }) => inviteMember(email, role),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: ['members'] })
      onInvited(
        result.temporary_password
          ? { email: result.member.email, password: result.temporary_password }
          : null,
      )
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    invite.mutate({
      email: String(form.get('email') ?? '').trim(),
      role: String(form.get('role') ?? 'developer'),
    })
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <TextInput
        label={t('login.email')}
        name="email"
        type="email"
        required
        dir="ltr"
        placeholder="carol@example.com"
      />
      <SelectInput label={t('team.role')} name="role" defaultValue="developer">
        {ROLES.map((r) => (
          <option key={r} value={r}>
            {t(`team.role.${r}`)}
          </option>
        ))}
      </SelectInput>
      <ErrorAlert error={invite.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={invite.isPending}>
          {invite.isPending ? t('team.inviting') : t('team.inviteButton')}
        </Button>
      </DialogFooter>
    </form>
  )
}

export function TeamPage() {
  const { me } = route.useRouteContext()
  const { t, tOr } = usePrefs()
  const { data: members } = useSuspenseQuery(membersQuery)
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['members'] })
  const [inviting, setInviting] = useState(false)
  const [invited, setInvited] = useState<{ email: string; password: string } | null>(null)

  const change = useMutation({
    mutationFn: ({ id, role }: { id: string; role: string }) => updateMember(id, role),
    onSettled: refresh,
  })
  const remove = useMutation({ mutationFn: removeMember, onSettled: refresh })
  const roleName = (role: string) => tOr(`team.role.${role}`, role)

  const columns: Column<Member>[] = [
    {
      id: 'name',
      header: t('projects.name'),
      sortValue: (m) => m.display_name ?? m.email,
      filterValue: (m) => `${m.display_name ?? ''} ${m.email}`,
      cell: (m) => (
        <div className="flex min-w-0 items-center gap-3">
          <Avatar aria-hidden="true" className="size-8">
            <AvatarFallback className="text-xs">{initials(m.display_name ?? m.email)}</AvatarFallback>
          </Avatar>
          <div className="min-w-0">
            <p className="flex items-center gap-2 truncate font-medium">
              <span dir="auto">{m.display_name ?? m.email}</span>
              {m.id === me.id && <Tag>{t('team.you')}</Tag>}
            </p>
            <p className="truncate text-muted-foreground text-xs">
              <span dir="ltr">{m.email}</span>
              {m.must_change_password && ` · ${t('team.invitationPending')}`}
            </p>
          </div>
        </div>
      ),
    },
    {
      id: 'role',
      header: t('team.role'),
      sortValue: (m) => ROLES.indexOf(m.role as (typeof ROLES)[number]),
      filterValue: (m) => `${roleName(m.role)} ${m.role}`,
      cell: (m) => (
        <NativeSelect
          size="sm"
          aria-label={fill(t('team.roleOf'), { email: m.email })}
          value={m.role}
          disabled={m.id === me.id || change.isPending}
          onChange={(e) => change.mutate({ id: m.id, role: e.target.value })}
        >
          {ROLES.map((r) => (
            <option key={r} value={r}>
              {roleName(r)}
            </option>
          ))}
        </NativeSelect>
      ),
    },
    {
      id: 'remove',
      header: t('ui.remove'),
      hideHeader: true,
      className: 'text-end',
      cell: (m) => (
        <Button
          variant="ghost"
          size="sm"
          className="text-destructive hover:text-destructive"
          disabled={m.id === me.id || remove.isPending}
          onClick={() => remove.mutate(m.id)}
        >
          {t('ui.remove')}
        </Button>
      ),
    },
  ]

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('nav.team')}
        description={t('team.lead')}
        actions={
          <FormDialog
            open={inviting}
            onOpenChange={setInviting}
            title={t('team.invite')}
            trigger={
              <Button>
                <UserPlusIcon aria-hidden="true" />
                {t('team.invite')}
              </Button>
            }
          >
            <InviteForm
              onInvited={(result) => {
                setInvited(result)
                setInviting(false)
              }}
            />
          </FormDialog>
        }
      />

      {invited && (
        <Notice tone="success">
          <span className="mb-2 block">
            {t('team.tempPasswordFor')} <strong dir="ltr">{invited.email}</strong>{' '}
            {t('team.tempPasswordNote')}
          </span>
          <Copyable value={invited.password} />
        </Notice>
      )}

      <Section title={fill(t('team.members'), { count: members.length })}>
        <ErrorAlert error={change.error ?? remove.error} />
        <DataTable
          label={t('nav.team')}
          columns={columns}
          rows={members}
          rowKey={(m) => m.id}
          empty={t('team.empty')}
          initialSort={{ id: 'name', desc: false }}
        />
      </Section>
    </div>
  )
}
