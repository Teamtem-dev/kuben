/**
 * Organization settings: single sign-on (as the sign-in page offers it) and
 * the CI trust policies that let GitHub Actions workflows deploy without a
 * stored token.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { PlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import {
  ConfirmAction,
  ErrorAlert,
  FormDialog,
  Loading,
  Notice,
  PageHeader,
  Section,
  SelectInput,
  Tag,
  TextareaInput,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { membersQuery, projectsQuery, ssoQuery } from '@/lib/api'
import { lines } from '@/lib/controls'
import { type CiPolicy, ciPoliciesQuery, createCiPolicy, revokeCiPolicy } from '@/lib/controls-api'
import { fill } from '@/lib/messages/pages'
import { duration } from '@/lib/ops'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'

function SsoCard() {
  const { t } = usePrefs()
  const sso = useQuery(ssoQuery)
  return (
    <Section title={t('settings.sso')} description={t('settings.ssoHint')}>
      {sso.isError ? (
        <ErrorAlert error={sso.error} />
      ) : sso.isPending ? (
        <Loading lines={2} />
      ) : (
        <dl className="grid gap-3 text-sm sm:grid-cols-[max-content_1fr] sm:gap-x-6">
          <dt className="text-muted-foreground">{t('settings.ssoState')}</dt>
          <dd>
            {sso.data.enabled ? (
              <ToneBadge tone="success">{t('settings.ssoOn')}</ToneBadge>
            ) : (
              <ToneBadge tone="neutral">{t('settings.ssoOff')}</ToneBadge>
            )}
          </dd>
          {sso.data.enabled && (
            <>
              <dt className="text-muted-foreground">{t('settings.ssoProvider')}</dt>
              <dd dir="auto">{sso.data.displayName ?? '—'}</dd>
              <dt className="text-muted-foreground">{t('settings.ssoStart')}</dt>
              <dd dir="ltr" className="text-start font-mono text-xs">
                {sso.data.startUrl ?? '—'}
              </dd>
            </>
          )}
        </dl>
      )}
    </Section>
  )
}

function CreatePolicyForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const projects = useQuery(projectsQuery)
  const create = useMutation({
    mutationFn: createCiPolicy,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['ci-policies'] })
      onDone()
    },
  })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const field = (key: string) => String(form.get(key) ?? '').trim()
    create.mutate({
      name: field('name'),
      project: field('project'),
      environment: field('environment') || null,
      repository: field('repository'),
      repositoryId: Number(field('repositoryId')),
      repositoryOwnerId: Number(field('repositoryOwnerId')),
      refs: lines(field('refs')),
      environments: lines(field('environments')),
      events: lines(field('events')),
      role: field('role') || null,
      tokenTtlSecs: field('ttl') ? Number(field('ttl')) : null,
    })
  }
  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <TextInput
          label={t('ci.name')}
          name="name"
          required
          maxLength={64}
          dir="ltr"
          placeholder="shop-deploy"
        />
        <SelectInput label={t('ci.project')} name="project" required defaultValue="">
          <option value="" disabled>
            {t('ci.chooseProject')}
          </option>
          {projects.data?.map((p) => (
            <option key={p.name} value={p.name}>
              {p.display_name} ({p.name})
            </option>
          ))}
        </SelectInput>
        <TextInput
          label={t('ci.environment')}
          name="environment"
          dir="ltr"
          placeholder="prod"
          hint={t('ci.environmentHint')}
        />
        <SelectInput label={t('ci.role')} name="role" defaultValue="developer" hint={t('ci.roleHint')}>
          <option value="developer">{t('team.role.developer')}</option>
          <option value="admin">{t('team.role.admin')}</option>
        </SelectInput>
        <TextInput
          label={t('ci.repository')}
          name="repository"
          required
          dir="ltr"
          placeholder="acme/shop"
          pattern="[^/\s]+/[^/\s]+"
        />
        <TextInput
          label={t('ci.ttl')}
          name="ttl"
          type="number"
          min={60}
          max={3600}
          placeholder="900"
          dir="ltr"
        />
        <TextInput
          label={t('ci.repositoryId')}
          name="repositoryId"
          type="number"
          min={1}
          required
          dir="ltr"
          hint={t('ci.idHint')}
        />
        <TextInput
          label={t('ci.repositoryOwnerId')}
          name="repositoryOwnerId"
          type="number"
          min={1}
          required
          dir="ltr"
        />
      </div>
      <TextareaInput
        label={t('ci.refs')}
        name="refs"
        required
        rows={2}
        dir="ltr"
        placeholder={'refs/heads/main\nrefs/tags/v*'}
        hint={t('ci.linesHint')}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        <TextareaInput
          label={t('ci.githubEnvironments')}
          name="environments"
          rows={2}
          dir="ltr"
          hint={t('ci.githubEnvironmentsHint')}
        />
        <TextareaInput label={t('ci.events')} name="events" rows={2} dir="ltr" hint={t('ci.eventsHint')} />
      </div>
      <ErrorAlert error={create.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending ? t('ui.creating') : t('ci.create')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function RevokePolicy({ policy }: { policy: CiPolicy }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const revoke = useMutation({
    mutationFn: () => revokeCiPolicy(policy.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['ci-policies'] }),
  })
  return (
    <ConfirmAction
      size="sm"
      variant="ghost"
      label={t('ci.revoke')}
      title={fill(t('ci.revokeTitle'), { name: policy.name })}
      description={t('ci.revokeConfirm')}
      pending={revoke.isPending}
      error={revoke.error}
      onConfirm={() => revoke.mutateAsync()}
    />
  )
}

function CiPoliciesCard() {
  const { t, locale } = usePrefs()
  const [adding, setAdding] = useState(false)
  const policies = useQuery(ciPoliciesQuery)
  const projects = useQuery(projectsQuery)
  const members = useQuery({ ...membersQuery, retry: false })
  const projectName = (id: string) => projects.data?.find((p) => p.uid === id)?.name ?? id.slice(0, 8)
  const memberName = (id: string) => members.data?.find((m) => m.id === id)?.email ?? id.slice(0, 8)
  const forbidden = policies.error instanceof ApiError && policies.error.status === 403

  const columns: Column<CiPolicy>[] = [
    {
      id: 'name',
      header: t('ci.name'),
      sortValue: (p) => p.name,
      filterValue: (p) => p.name,
      cell: (p) => (
        <span dir="ltr" className="font-medium font-mono text-xs">
          {p.name}
        </span>
      ),
    },
    {
      id: 'repository',
      header: t('ci.repository'),
      sortValue: (p) => p.repository,
      filterValue: (p) => `${p.repository} ${p.refs.join(' ')} ${p.environments.join(' ')}`,
      className: 'whitespace-normal',
      cell: (p) => (
        <div dir="ltr" className="space-y-1 text-start">
          <p className="font-mono text-xs">{p.repository}</p>
          <p className="flex flex-wrap gap-1">
            {p.refs.map((r) => (
              <Tag key={r}>{r}</Tag>
            ))}
          </p>
        </div>
      ),
    },
    {
      id: 'project',
      header: t('ci.project'),
      sortValue: (p) => projectName(p.project),
      filterValue: (p) => projectName(p.project),
      cell: (p) => (
        <span dir="ltr" className="font-mono text-xs">
          {projectName(p.project)}
          {p.environment && <span className="text-muted-foreground"> · {t('ci.oneEnvironment')}</span>}
        </span>
      ),
    },
    {
      id: 'role',
      header: t('ci.role'),
      sortValue: (p) => p.role,
      filterValue: (p) => p.role,
      cell: (p) => (
        <span>
          {t(p.role === 'admin' ? 'team.role.admin' : 'team.role.developer')}
          <span className="block text-muted-foreground text-xs">{duration(p.tokenTtlSecs, locale)}</span>
        </span>
      ),
    },
    {
      id: 'created',
      header: t('ci.created'),
      sortValue: (p) => p.createdAt,
      className: 'text-muted-foreground text-xs',
      cell: (p) => (
        <>
          <time dateTime={new Date(p.createdAt).toISOString()}>
            {new Date(p.createdAt).toLocaleString(locale)}
          </time>
          <span dir="ltr" className="block">
            {memberName(p.createdBy)}
          </span>
        </>
      ),
    },
    {
      id: 'state',
      header: t('ci.state'),
      sortValue: (p) => (p.revokedAt ? 1 : 0),
      cell: (p) =>
        p.revokedAt ? (
          <ToneBadge tone="neutral">{t('ci.revoked')}</ToneBadge>
        ) : (
          <ToneBadge tone="success">{t('ci.active')}</ToneBadge>
        ),
    },
    {
      id: 'actions',
      header: t('ci.revoke'),
      hideHeader: true,
      className: 'text-end',
      cell: (p) => !p.revokedAt && <RevokePolicy policy={p} />,
    },
  ]

  return (
    <Section
      title={t('ci.title')}
      description={t('ci.hint')}
      actions={
        !forbidden && (
          <FormDialog
            open={adding}
            onOpenChange={setAdding}
            title={t('ci.add')}
            description={t('ci.addHint')}
            className="sm:max-w-2xl"
            trigger={
              <Button size="sm">
                <PlusIcon aria-hidden="true" />
                {t('ci.add')}
              </Button>
            }
          >
            <CreatePolicyForm onDone={() => setAdding(false)} />
          </FormDialog>
        )
      }
    >
      {forbidden ? (
        <Notice>{t('ci.forbidden')}</Notice>
      ) : policies.isError ? (
        <ErrorAlert error={policies.error} />
      ) : (
        <DataTable
          label={t('ci.title')}
          columns={columns}
          rows={policies.data}
          rowKey={(p) => p.id}
          loading={policies.isPending}
          empty={t('ci.empty')}
          initialSort={{ id: 'state', desc: false }}
        />
      )}
    </Section>
  )
}

export function SettingsPage() {
  const { t } = usePrefs()
  return (
    <div className="space-y-6">
      <PageHeader title={t('nav.settings')} description={t('settings.lead')} />
      <SsoCard />
      <CiPoliciesCard />
    </div>
  )
}
