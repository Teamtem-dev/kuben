import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  Card,
  ConfirmDelete,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextArea,
  TextField,
} from '../components/ui'
import {
  appQuery,
  checkDomains,
  deleteApp,
  environmentsQuery,
  type PromoteResult,
  promoteApp,
  releasesQuery,
  restartApp,
  rollbackApp,
  runApp,
  type UpdateApp,
  updateApp,
  type Volume,
} from '../lib/api'
import { formatEnvLines, parseEnvLines } from '../lib/env'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'
import { DeploymentsCard } from './app-deployments'
import { LiveLogs } from './app-logs'
import { DetachCard, DnsCard, ImagePolicyCard, UsageCard } from './ops/app-ops'

const route = getRouteApi('/_authed/projects/$project/$environment/$app')

export function AppPage() {
  const { project, environment, app } = route.useParams()
  const { data } = useSuspenseQuery(appQuery(project, environment, app))
  const a = data.app
  const web = a.processes[0]
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { t } = usePrefs()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })

  const update = useMutation({
    mutationFn: (body: UpdateApp) => updateApp(project, environment, app, body),
    onSuccess: refresh,
  })
  const restart = useMutation({ mutationFn: () => restartApp(project, environment, app), onSuccess: refresh })
  const run = useMutation({ mutationFn: () => runApp(project, environment, app) })
  const [deleteVolumes, setDeleteVolumes] = useState(false)
  const scheduled = a.processes.find((p) => p.schedule)
  const remove = useMutation({
    mutationFn: () => deleteApp(project, environment, app, deleteVolumes),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['apps', project, environment] })
      await navigate({ to: '/projects/$project/$environment', params: { project, environment } })
    },
  })

  return (
    <section className="space-y-6">
      <PageHeader
        crumbs={
          <>
            <Link to="/" className="hover:text-fg">
              {t('nav.projects')}
            </Link>
            <span>/</span>
            <Link to="/projects/$project" params={{ project }} className="hover:text-fg">
              {project}
            </Link>
            <span>/</span>
            <Link
              to="/projects/$project/$environment"
              params={{ project, environment }}
              className="hover:text-fg"
            >
              {environment}
            </Link>
          </>
        }
        title={
          <span className="flex items-center gap-3">
            <span dir="auto">{a.name}</span> <Status ready={a.ready} label={a.reason} />
          </span>
        }
        subtitle={
          a.url ? (
            <a href={a.url} target="_blank" rel="noreferrer" dir="ltr" className="text-link hover:underline">
              {a.url}
            </a>
          ) : (
            t('app.notExposed')
          )
        }
        actions={
          <>
            {scheduled && (
              <Button variant="secondary" disabled={run.isPending} onClick={() => run.mutate()}>
                {run.isPending ? t('app.starting') : t('app.runNow')}
              </Button>
            )}
            <Link
              to="/projects/$project/$environment/$app/doctor"
              params={{ project, environment, app }}
              className="inline-flex items-center rounded-lg border border-line px-3 py-1.5 font-medium text-sm transition hover:bg-hover"
            >
              {t('doctor.open')}
            </Link>
            <Button variant="secondary" disabled={restart.isPending} onClick={() => restart.mutate()}>
              {restart.isPending ? t('app.restarting') : t('app.restart')}
            </Button>
          </>
        }
      />
      {scheduled && (
        <p className="text-muted text-sm">
          {t('app.scheduledJob')}{' '}
          <code dir="ltr" className="font-mono">
            {scheduled.schedule}
          </code>
          {run.data && <span className="text-ok"> · {fill(t('app.started'), { job: run.data.job })}</span>}
        </p>
      )}
      <ErrorNote error={run.error} />
      {!a.ready && a.message && (
        <p dir="auto" className="rounded-lg bg-warn/10 px-3 py-2 text-warn text-sm">
          {a.message}
        </p>
      )}
      <ErrorNote error={restart.error} />

      <div className="grid gap-4 lg:grid-cols-2">
        <DeployCard
          image={a.image ?? ''}
          pending={update.isPending}
          error={update.error}
          onDeploy={(image) => update.mutate({ image })}
        />
        <ScaleCard
          min={web?.min_replicas ?? 1}
          max={web?.max_replicas ?? 1}
          size={web?.size ?? 'small'}
          pending={update.isPending}
          onSave={(body) => update.mutate(body)}
        />
      </div>

      <DeploymentsCard project={project} environment={environment} app={app} />

      <div className="grid gap-4 lg:grid-cols-2">
        <ReleasesCard project={project} environment={environment} app={app} />
        <PromoteCard project={project} environment={environment} app={app} />
      </div>

      <Card title={fill(t('app.pods'), { count: data.pods.length })}>
        {data.pods.length === 0 ? (
          <p className="text-subtle text-sm">{t('app.noPods')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-start text-sm">
              <thead className="text-subtle text-xs">
                <tr>
                  <th className="pb-2 font-medium">{t('app.pod')}</th>
                  <th className="pb-2 font-medium">{t('app.status')}</th>
                  <th className="pb-2 font-medium">{t('app.restarts')}</th>
                  <th className="pb-2 font-medium">{t('app.node')}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {data.pods.map((pod) => (
                  <tr key={pod.name}>
                    <td className="py-2 pe-4 font-mono text-xs">{pod.name}</td>
                    <td className="py-2 pe-4">
                      <Status ready={pod.ready} label={pod.reason ?? pod.phase} />
                    </td>
                    <td className="py-2 pe-4">{pod.restarts}</td>
                    <td className="py-2 font-mono text-subtle text-xs">{pod.node ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <EnvCard
        text={formatEnvLines(a.env)}
        pending={update.isPending}
        error={update.error}
        onSave={(env) => update.mutate({ env })}
      />

      <LiveLogs
        project={project}
        environment={environment}
        app={app}
        processes={a.processes.filter((p) => !p.schedule).map((p) => p.name)}
      />

      <DomainsCard
        project={project}
        environment={environment}
        app={app}
        domains={a.domains}
        pending={update.isPending}
        onSave={(domains) => update.mutate({ domains })}
      />

      <DnsCard project={project} environment={environment} app={app} domains={a.domains} />

      <UsageCard project={project} environment={environment} app={app} />

      <ImagePolicyCard project={project} environment={environment} app={app} />

      {a.volumes.length > 0 && <VolumesCard volumes={a.volumes} />}

      {a.volumes.length > 0 && (
        <label className="flex items-center gap-2 text-muted text-sm">
          <input
            type="checkbox"
            checked={deleteVolumes}
            onChange={(e) => setDeleteVolumes(e.target.checked)}
          />
          {t('app.deleteVolumes')}
        </label>
      )}
      <DetachCard project={project} environment={environment} app={app} />
      <ConfirmDelete
        name={a.name}
        what={t('app.what')}
        pending={remove.isPending}
        error={remove.error}
        onConfirm={() => remove.mutate()}
      />
    </section>
  )
}

function DeployCard({
  image,
  pending,
  error,
  onDeploy,
}: {
  image: string
  pending: boolean
  error: unknown
  onDeploy: (image: string) => void
}) {
  const { t } = usePrefs()
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const next = String(new FormData(event.currentTarget).get('image') ?? '').trim()
    if (next) onDeploy(next)
  }
  return (
    <Card title={t('environment.image')}>
      <form onSubmit={onSubmit} className="space-y-3">
        <TextField key={image} label={t('app.deployImage')} name="image" defaultValue={image} required />
        <div className="flex items-center gap-3">
          <Button type="submit" disabled={pending}>
            {t('ui.deploy')}
          </Button>
          <ErrorNote error={error} />
        </div>
      </form>
    </Card>
  )
}

function ScaleCard({
  min,
  max,
  size,
  pending,
  onSave,
}: {
  min: number
  max: number
  size: string
  pending: boolean
  onSave: (body: UpdateApp) => void
}) {
  const { t } = usePrefs()
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const replicas = Number(form.get('replicas'))
    const maxReplicas = Number(form.get('max_replicas'))
    onSave({ replicas, max_replicas: Math.max(replicas, maxReplicas) })
  }
  return (
    <Card title={`${t('app.scale')} · ${size}`}>
      <form onSubmit={onSubmit} key={`${min}-${max}`} className="grid grid-cols-2 gap-3">
        <TextField
          label={t('environment.replicas')}
          name="replicas"
          type="number"
          min={0}
          max={50}
          defaultValue={min}
        />
        <TextField
          label={t('environment.autoscale')}
          name="max_replicas"
          type="number"
          min={0}
          max={50}
          defaultValue={max}
          hint={t('app.autoscaleHint')}
        />
        <div className="col-span-2">
          <Button type="submit" variant="secondary" disabled={pending}>
            {t('ui.save')}
          </Button>
        </div>
      </form>
    </Card>
  )
}

function EnvCard({
  text,
  pending,
  error,
  onSave,
}: {
  text: string
  pending: boolean
  error: unknown
  onSave: (env: ReturnType<typeof parseEnvLines>['vars']) => void
}) {
  const { t } = usePrefs()
  const [parseError, setParseError] = useState<string | null>(null)
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const { vars, errors } = parseEnvLines(String(new FormData(event.currentTarget).get('env') ?? ''))
    setParseError(errors.length ? errors.join('; ') : null)
    if (!errors.length) onSave(vars)
  }
  return (
    <Card title={t('environment.envVars')}>
      <form onSubmit={onSubmit} className="space-y-3">
        <TextArea
          key={text}
          label={t('app.variables')}
          name="env"
          defaultValue={text}
          hint={t('app.variablesHint')}
        />
        <div className="flex items-center gap-3">
          <Button type="submit" variant="secondary" disabled={pending}>
            {t('app.saveAndRollOut')}
          </Button>
          {parseError && <ErrorNote error={new Error(parseError)} />}
          <ErrorNote error={error} />
        </div>
      </form>
    </Card>
  )
}

function ReleasesCard({ project, environment, app }: { project: string; environment: string; app: string }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const releases = useQuery({ ...releasesQuery(project, environment, app), retry: false })
  const rollback = useMutation({
    mutationFn: (revision: number) => rollbackApp(project, environment, app, revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] }),
  })
  return (
    <Card title={t('app.releases')}>
      {releases.isError ? (
        <ErrorNote error={releases.error} />
      ) : !releases.data?.length ? (
        <p className="text-subtle text-sm">{t('app.noReleases')}</p>
      ) : (
        <ul className="max-h-80 divide-y divide-line-soft overflow-y-auto">
          {releases.data.map((r) => (
            <li key={r.revision} className="flex items-center justify-between gap-3 py-2 text-sm">
              <div className="min-w-0">
                <p className="truncate">
                  <span className="font-mono">#{r.revision}</span> <Badge>{r.reason}</Badge>{' '}
                  <span dir="ltr" className="font-mono text-muted text-xs">
                    {r.image ?? '—'}
                  </span>
                </p>
                <p className="truncate text-subtle text-xs">
                  {new Date(r.created_at).toLocaleString(locale)}
                  {r.actor ? ` · ${r.actor}` : ''}
                  {r.note ? (
                    <>
                      {' · '}
                      <span dir="auto">{r.note}</span>
                    </>
                  ) : (
                    ''
                  )}
                </p>
              </div>
              {r.current ? (
                <span className="text-ok text-xs">{t('app.current')}</span>
              ) : (
                <Button
                  variant="secondary"
                  disabled={rollback.isPending}
                  onClick={() => rollback.mutate(r.revision)}
                >
                  {t('app.rollBack')}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <ErrorNote error={rollback.error} />
    </Card>
  )
}

function PromoteCard({ project, environment, app }: { project: string; environment: string; app: string }) {
  const { t } = usePrefs()
  const environments = useQuery(environmentsQuery(project))
  const targets = (environments.data ?? []).filter((e) => e.name !== environment)
  const [target, setTarget] = useState('')
  const [result, setResult] = useState<PromoteResult | null>(null)
  const promote = useMutation({
    mutationFn: (dryRun: boolean) => promoteApp(project, environment, app, target, dryRun),
    onSuccess: setResult,
  })
  if (targets.length === 0) return null
  const previewed = result?.dry_run === true
  return (
    <Card title={t('app.promote')}>
      <div className="flex flex-wrap items-end gap-3">
        <Select
          label={t('app.targetEnvironment')}
          value={target}
          onChange={(e) => {
            setTarget(e.target.value)
            setResult(null)
          }}
        >
          <option value="">{t('app.choose')}</option>
          {targets.map((e) => (
            <option key={e.name} value={e.name}>
              {e.name} ({e.env_type})
            </option>
          ))}
        </Select>
        <Button
          variant="secondary"
          disabled={!target || promote.isPending}
          onClick={() => promote.mutate(true)}
        >
          {t('app.previewChanges')}
        </Button>
        <Button disabled={!previewed || promote.isPending} onClick={() => promote.mutate(false)}>
          {t('app.promote')}
        </Button>
      </div>
      <p className="mt-2 text-subtle text-xs">{t('app.promoteHint')}</p>
      {result && (
        <div className="mt-3 space-y-2 text-sm">
          {result.dry_run ? (
            <p className="text-fg-soft">
              {result.changes.length
                ? fill(t('app.changesIn'), { target })
                : fill(t('app.upToDate'), { target })}
            </p>
          ) : (
            <p className="text-ok">
              {fill(t('app.promoted'), { target })}
              {result.created ? ` (${t('app.appCreated')})` : ''}.
            </p>
          )}
          <ul dir="ltr" className="list-disc space-y-0.5 ps-5 text-start font-mono text-xs">
            {result.changes.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
          {result.warnings.map((w) => (
            <p key={w} dir="auto" className="text-warn text-xs">
              {w}
            </p>
          ))}
        </div>
      )}
      <ErrorNote error={promote.error} />
    </Card>
  )
}

const dnsColor: Record<string, string> = {
  ok: 'text-ok',
  mismatch: 'text-danger',
  unresolved: 'text-warn',
  unknown: 'text-muted',
}

function DomainsCard({
  project,
  environment,
  app,
  domains,
  pending,
  onSave,
}: {
  project: string
  environment: string
  app: string
  domains: readonly string[]
  pending: boolean
  onSave: (domains: string[]) => void
}) {
  const { t, tOr } = usePrefs()
  const check = useMutation({ mutationFn: () => checkDomains(project, environment, app) })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const text = String(new FormData(event.currentTarget).get('domains') ?? '')
    onSave(text.split(/[\s,]+/).filter(Boolean))
  }
  return (
    <Card
      title={t('environment.domains')}
      actions={
        <Button variant="secondary" disabled={check.isPending} onClick={() => check.mutate()}>
          {check.isPending ? t('app.checking') : t('app.checkDns')}
        </Button>
      }
    >
      <form onSubmit={onSubmit} className="flex flex-wrap items-end gap-3">
        <div className="min-w-64 flex-1">
          <TextField
            key={domains.join(' ')}
            label={t('app.customDomains')}
            name="domains"
            defaultValue={domains.join(' ')}
            placeholder="api.example.com www.example.com"
            hint={t('app.customDomainsHint')}
          />
        </div>
        <Button type="submit" variant="secondary" disabled={pending}>
          {t('ui.save')}
        </Button>
      </form>
      {check.data && (
        <ul className="mt-3 space-y-1 text-sm">
          {check.data.map((d) => (
            <li key={d.host} className="flex flex-wrap items-center gap-2">
              <span dir="ltr" className="font-mono">
                {d.host}
              </span>
              <span className={dnsColor[d.status] ?? ''}>{tOr(`app.dns.${d.status}`, d.status)}</span>
              <span dir="auto" className="text-subtle text-xs">
                {d.message}
              </span>
            </li>
          ))}
        </ul>
      )}
      <ErrorNote error={check.error} />
    </Card>
  )
}

function VolumesCard({ volumes }: { volumes: readonly Volume[] }) {
  const { t } = usePrefs()
  return (
    <Card title={t('app.volumes')}>
      <ul className="space-y-1 text-sm">
        {volumes.map((v) => (
          <li key={v.name}>
            <span dir="ltr" className="font-mono">
              {v.mount_path}
            </span>{' '}
            <Badge>{v.size}</Badge>{' '}
            <span className="text-subtle text-xs">
              {v.name} · {t('app.volumeKept')}
            </span>
          </li>
        ))}
      </ul>
    </Card>
  )
}
