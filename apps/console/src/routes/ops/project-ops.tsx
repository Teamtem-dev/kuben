import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLinkIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  CheckboxField,
  ConfirmAction,
  EmptyState,
  ErrorAlert,
  Loading,
  Section,
  SelectInput,
  SwitchField,
  Tag,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { environmentsQuery } from '@/lib/api'
import { fill } from '@/lib/messages/pages'
import { duration, when } from '@/lib/ops'
import {
  deleteStatusPage,
  destroyPreview,
  extendPreview,
  type Preview,
  previewPolicyQuery,
  previewsQuery,
  putPreviewPolicy,
  putStatusPage,
  statusPageQuery,
} from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'

function PolicyForm({ project }: { project: string }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const policy = useQuery(previewPolicyQuery(project))
  const environments = useQuery(environmentsQuery(project))
  const save = useMutation({
    mutationFn: (form: FormData) =>
      putPreviewPolicy(project, {
        enabled: form.get('enabled') === 'on',
        sourceEnvironment: String(form.get('source') ?? ''),
        ttlHours: Number(form.get('ttl')),
        maxActive: Number(form.get('max')),
        allowForks: form.get('forks') === 'on',
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['previews', project] }),
  })
  if (!policy.data || !environments.data) return <ErrorAlert error={policy.error ?? environments.error} />
  const p = policy.data
  const sources = environments.data.filter((e) => e.env_type !== 'preview')
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    save.mutate(new FormData(e.currentTarget))
  }
  return (
    <form onSubmit={submit} className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <SelectInput
          label={t('previews.source')}
          name="source"
          defaultValue={p.sourceEnvironment ?? sources[0]?.name}
        >
          {sources.map((e) => (
            <option key={e.name} value={e.name}>
              {e.name}
            </option>
          ))}
        </SelectInput>
        <TextInput
          label={t('previews.ttl')}
          name="ttl"
          type="number"
          min={1}
          max={720}
          defaultValue={p.ttlHours}
        />
        <TextInput
          label={t('previews.max')}
          name="max"
          type="number"
          min={1}
          max={100}
          defaultValue={p.maxActive}
        />
      </div>
      <div className="space-y-2">
        <div className="flex flex-wrap gap-x-6 gap-y-2">
          <CheckboxField label={t('previews.enabled')} name="enabled" defaultChecked={p.enabled} />
          <CheckboxField label={t('previews.allowForks')} name="forks" defaultChecked={p.allowForks} />
        </div>
        <p className="text-muted-foreground text-xs">{t('previews.forksHint')}</p>
      </div>
      <ErrorAlert error={save.error} />
      <div>
        <Button type="submit" variant="secondary" disabled={save.isPending || sources.length === 0}>
          {t('ops.save')}
        </Button>
      </div>
    </form>
  )
}

function PreviewRow({ project, preview }: { project: string; preview: Preview }) {
  const { t, tOr, locale } = usePrefs()
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['previews', project] })
  const extend = useMutation({
    mutationFn: (keep: boolean) => extendPreview(project, preview.environment, 24, keep),
    onSuccess: refresh,
  })
  const destroy = useMutation({
    mutationFn: () => destroyPreview(project, preview.environment),
    onSuccess: refresh,
  })
  const active = preview.state === 'active'
  return (
    <li className="space-y-2 py-3 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1 text-sm">
          <p className="flex flex-wrap items-center gap-2">
            {active ? (
              <Link
                to="/projects/$project/$environment"
                params={{ project, environment: preview.environment }}
                className="font-medium text-link hover:underline"
              >
                {preview.environment}
              </Link>
            ) : (
              <span className="font-medium">{preview.environment}</span>
            )}
            <ToneBadge tone={preview.state}>
              {tOr(`previews.state.${preview.state}`, preview.state)}
            </ToneBadge>
            {!preview.trusted && <ToneBadge tone="warning">{t('previews.fork')}</ToneBadge>}
          </p>
          <p dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
            {preview.repository}#{preview.pullRequest} · {preview.branch} · {preview.commit.slice(0, 12)}
          </p>
          <p className="text-muted-foreground text-xs">
            {active
              ? preview.autoDelete
                ? `${t('previews.remaining')} ${duration(preview.remainingSeconds, locale)}`
                : t('previews.kept')
              : `${tOr(`previews.reason.${preview.closeReason}`, preview.closeReason ?? '')} · ${when(preview.closedAt, locale)}`}
          </p>
        </div>
        {active && (
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={extend.isPending}
              onClick={() => extend.mutate(false)}
            >
              {t('previews.extend')}
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={extend.isPending}
              onClick={() => extend.mutate(true)}
            >
              {t('previews.keep')}
            </Button>
            <ConfirmAction
              size="sm"
              label={t('previews.destroy')}
              title={t('previews.destroy')}
              description={fill(t('previews.destroyConfirm'), { name: preview.environment })}
              pending={destroy.isPending}
              error={destroy.error}
              onConfirm={() => destroy.mutateAsync()}
            />
          </div>
        )}
      </div>
      <ErrorAlert error={extend.error} />
    </li>
  )
}

/** A project's preview settings and previews (M5.1). */
export function PreviewsCard({ project }: { project: string }) {
  const { t } = usePrefs()
  const [all, setAll] = useState(false)
  const previews = useQuery({ ...previewsQuery(project, all), refetchInterval: 60_000 })
  return (
    <Section
      title={t('previews.title')}
      actions={<SwitchField label={t('previews.showClosed')} checked={all} onCheckedChange={setAll} />}
    >
      <PolicyForm project={project} />
      <ErrorAlert error={previews.error} />
      {previews.isPending && <Loading />}
      {previews.data?.length === 0 && <EmptyState>{t('previews.empty')}</EmptyState>}
      {previews.data && previews.data.length > 0 && (
        <ul className="divide-y border-t pt-4">
          {previews.data.map((p) => (
            <PreviewRow key={p.environment} project={project} preview={p} />
          ))}
        </ul>
      )}
    </Section>
  )
}

/** A project's public status page (M5.3). */
export function StatusPageCard({ project }: { project: string }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const page = useQuery(statusPageQuery(project))
  const environments = useQuery(environmentsQuery(project))
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['status-page', project] })
  const save = useMutation({
    mutationFn: (form: FormData) =>
      putStatusPage(project, {
        slug: String(form.get('slug') ?? ''),
        title: String(form.get('title') ?? ''),
        enabled: form.get('enabled') === 'on',
        environments: form.getAll('environments').map(String),
      }),
    onSuccess: refresh,
  })
  const remove = useMutation({ mutationFn: () => deleteStatusPage(project), onSuccess: refresh })
  if (page.isPending || !environments.data) return null
  const current = page.data
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    save.mutate(new FormData(e.currentTarget))
  }
  return (
    <Section
      title={t('statusPage.title')}
      description={t('statusPage.privacy')}
      actions={
        current && (
          <Button variant="outline" size="sm" asChild>
            <a href={current.path} target="_blank" rel="noopener noreferrer">
              {t('statusPage.open')}
              <ExternalLinkIcon aria-hidden="true" />
            </a>
          </Button>
        )
      }
    >
      <form onSubmit={submit} key={current?.updatedAt ?? 'new'} className="grid gap-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput
            label={t('statusPage.slug')}
            name="slug"
            required
            dir="ltr"
            defaultValue={current?.slug ?? project}
          />
          <TextInput
            label={t('statusPage.pageTitle')}
            name="title"
            required
            defaultValue={current?.title ?? project}
          />
        </div>
        <fieldset className="space-y-3">
          <legend className="mb-3 font-medium text-sm">{t('statusPage.environments')}</legend>
          <div className="flex flex-wrap gap-x-6 gap-y-2">
            {environments.data
              .filter((e) => e.env_type !== 'preview')
              .map((e) => (
                <CheckboxField
                  key={e.name}
                  name="environments"
                  value={e.name}
                  defaultChecked={
                    current ? current.environments.includes(e.name) : e.env_type === 'production'
                  }
                  label={
                    <>
                      {e.name} <Tag>{e.env_type}</Tag>
                    </>
                  }
                />
              ))}
          </div>
        </fieldset>
        <CheckboxField
          label={t('statusPage.published')}
          name="enabled"
          defaultChecked={current?.enabled ?? true}
        />
        <ErrorAlert error={save.error} />
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" variant={current ? 'secondary' : 'default'} disabled={save.isPending}>
            {current ? t('ops.save') : t('statusPage.publish')}
          </Button>
          {current && (
            <>
              <ConfirmAction
                variant="ghost"
                label={t('statusPage.takeDown')}
                title={t('statusPage.takeDown')}
                description={t('statusPage.takeDownConfirm')}
                pending={remove.isPending}
                error={remove.error}
                onConfirm={() => remove.mutateAsync()}
              />
              <span className="text-muted-foreground text-xs">
                {t('ops.updated')} {when(current.updatedAt, locale)}
              </span>
            </>
          )}
        </div>
      </form>
    </Section>
  )
}
