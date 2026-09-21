import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Pill } from '../../components/ops'
import { Badge, Button, Card, ErrorNote, Select, TextField } from '../../components/ui'
import { environmentsQuery } from '../../lib/api'
import { duration, when } from '../../lib/ops'
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
} from '../../lib/ops-api'
import { usePrefs } from '../../lib/prefs'

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
  if (!policy.data || !environments.data) return <ErrorNote error={policy.error ?? environments.error} />
  const p = policy.data
  const sources = environments.data.filter((e) => e.env_type !== 'preview')
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    save.mutate(new FormData(e.currentTarget))
  }
  return (
    <form onSubmit={submit} className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-3">
        <Select
          label={t('previews.source')}
          name="source"
          defaultValue={p.sourceEnvironment ?? sources[0]?.name}
        >
          {sources.map((e) => (
            <option key={e.name} value={e.name}>
              {e.name}
            </option>
          ))}
        </Select>
        <TextField
          label={t('previews.ttl')}
          name="ttl"
          type="number"
          min={1}
          max={720}
          defaultValue={p.ttlHours}
        />
        <TextField
          label={t('previews.max')}
          name="max"
          type="number"
          min={1}
          max={100}
          defaultValue={p.maxActive}
        />
      </div>
      <div className="flex flex-wrap gap-4 text-sm">
        <label className="flex items-center gap-2">
          <input type="checkbox" name="enabled" defaultChecked={p.enabled} />
          {t('previews.enabled')}
        </label>
        <label className="flex items-center gap-2">
          <input type="checkbox" name="forks" defaultChecked={p.allowForks} />
          {t('previews.allowForks')}
        </label>
      </div>
      <p className="text-subtle text-xs">{t('previews.forksHint')}</p>
      <ErrorNote error={save.error} />
      <Button type="submit" disabled={save.isPending || sources.length === 0}>
        {t('ops.save')}
      </Button>
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
    <li className="flex flex-wrap items-start justify-between gap-3 py-3">
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
          <Pill tone={preview.state}>{tOr(`previews.state.${preview.state}`, preview.state)}</Pill>
          {!preview.trusted && <Pill tone="warning">{t('previews.fork')}</Pill>}
        </p>
        <p dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
          {preview.repository}#{preview.pullRequest} · {preview.branch} · {preview.commit.slice(0, 12)}
        </p>
        <p className="text-subtle text-xs">
          {active
            ? preview.autoDelete
              ? `${t('previews.remaining')} ${duration(preview.remainingSeconds, locale)}`
              : t('previews.kept')
            : `${tOr(`previews.reason.${preview.closeReason}`, preview.closeReason ?? '')} · ${when(preview.closedAt, locale)}`}
        </p>
      </div>
      {active && (
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" disabled={extend.isPending} onClick={() => extend.mutate(false)}>
            {t('previews.extend')}
          </Button>
          <Button variant="secondary" disabled={extend.isPending} onClick={() => extend.mutate(true)}>
            {t('previews.keep')}
          </Button>
          <Button variant="danger" disabled={destroy.isPending} onClick={() => destroy.mutate()}>
            {t('previews.destroy')}
          </Button>
        </div>
      )}
      <ErrorNote error={extend.error ?? destroy.error} />
    </li>
  )
}

/** A project's preview settings and previews (M5.1). */
export function PreviewsCard({ project }: { project: string }) {
  const { t } = usePrefs()
  const [all, setAll] = useState(false)
  const previews = useQuery({ ...previewsQuery(project, all), refetchInterval: 60_000 })
  return (
    <Card
      title={t('previews.title')}
      actions={
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
          {t('previews.showClosed')}
        </label>
      }
    >
      <div className="space-y-4">
        <PolicyForm project={project} />
        <ErrorNote error={previews.error} />
        {previews.data?.length === 0 && (
          <p className="text-muted-foreground text-sm">{t('previews.empty')}</p>
        )}
        <ul className="divide-y divide-line-soft">
          {previews.data?.map((p) => (
            <PreviewRow key={`${p.environment}`} project={project} preview={p} />
          ))}
        </ul>
      </div>
    </Card>
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
    <Card title={t('statusPage.title')}>
      <form onSubmit={submit} className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-2">
          <TextField
            label={t('statusPage.slug')}
            name="slug"
            required
            dir="ltr"
            defaultValue={current?.slug ?? project}
          />
          <TextField
            label={t('statusPage.pageTitle')}
            name="title"
            required
            defaultValue={current?.title ?? project}
          />
        </div>
        <fieldset className="space-y-1">
          <legend className="font-medium text-sm">{t('statusPage.environments')}</legend>
          <div className="flex flex-wrap gap-3">
            {environments.data
              .filter((e) => e.env_type !== 'preview')
              .map((e) => (
                <label key={e.name} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    name="environments"
                    value={e.name}
                    defaultChecked={
                      current ? current.environments.includes(e.name) : e.env_type === 'production'
                    }
                  />
                  {e.name} <Badge>{e.env_type}</Badge>
                </label>
              ))}
          </div>
        </fieldset>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" name="enabled" defaultChecked={current?.enabled ?? true} />
          {t('statusPage.published')}
        </label>
        <p className="text-subtle text-xs">{t('statusPage.privacy')}</p>
        <ErrorNote error={save.error ?? remove.error} />
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" disabled={save.isPending}>
            {current ? t('ops.save') : t('statusPage.publish')}
          </Button>
          {current && (
            <>
              <a
                href={current.path}
                target="_blank"
                rel="noopener noreferrer"
                className="text-link text-sm hover:underline"
              >
                {t('statusPage.open')}
              </a>
              <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {t('statusPage.takeDown')}
              </Button>
              <span className="text-subtle text-xs">
                {t('ops.updated')} {when(current.updatedAt, locale)}
              </span>
            </>
          )}
        </div>
      </form>
    </Card>
  )
}
