import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { ExternalLinkIcon, PlayIcon, RotateCwIcon, StethoscopeIcon } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import {
  ConfirmDelete,
  ErrorAlert,
  Notice,
  PageHeader,
  Section,
  SelectInput,
  StatusBadge,
  Tag,
  TextareaInput,
  TextInput,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  appQuery,
  checkDomains,
  deleteApp,
  environmentsQuery,
  type PromoteResult,
  promoteApp,
  type Release,
  releasesQuery,
  restartApp,
  rollbackApp,
  runApp,
  type UpdateApp,
  updateApp,
  type Volume,
} from '@/lib/api'
import { APP_TABS, type AppTab } from '@/lib/app-tabs'
import { formatEnvLines, parseEnvLines } from '@/lib/env'
import type { MessageKey } from '@/lib/messages'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { DeploymentsCard } from './app-deployments'
import { LiveLogs } from './app-logs'
import { DetachCard, DnsCard, ImagePolicyCard, UsageCard } from './ops/app-ops'

const route = getRouteApi('/_authed/projects/$project/$environment/$app')

const TAB_LABELS: Record<AppTab, MessageKey> = {
  overview: 'app.tab.overview',
  deployments: 'deployments.title',
  releases: 'app.releases',
  logs: 'logs.title',
  settings: 'app.tab.settings',
}

export function AppPage() {
  const { project, environment, app } = route.useParams()
  const { tab = 'overview' } = route.useSearch()
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
  const deleteVolumesId = useId()
  const scheduled = a.processes.find((p) => p.schedule)
  const remove = useMutation({
    mutationFn: () => deleteApp(project, environment, app, deleteVolumes),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['apps', project, environment] })
      await navigate({ to: '/projects/$project/$environment', params: { project, environment } })
    },
  })

  const openTab = (value: string) =>
    navigate({
      to: '/projects/$project/$environment/$app',
      params: { project, environment, app },
      search: value === 'overview' ? {} : { tab: value as Exclude<AppTab, 'overview'> },
      replace: true,
    })

  return (
    <div className="space-y-6">
      <PageHeader
        title={
          <>
            <span dir="auto">{a.name}</span> <StatusBadge ready={a.ready} label={a.reason} />
          </>
        }
        description={
          a.url ? (
            <a
              href={a.url}
              target="_blank"
              rel="noreferrer"
              dir="ltr"
              className="inline-flex items-center gap-1 text-link hover:underline"
            >
              {a.url}
              <ExternalLinkIcon aria-hidden="true" className="size-3.5" />
            </a>
          ) : (
            t('app.notExposed')
          )
        }
        actions={
          <>
            {scheduled && (
              <Button variant="outline" disabled={run.isPending} onClick={() => run.mutate()}>
                <PlayIcon aria-hidden="true" />
                {run.isPending ? t('app.starting') : t('app.runNow')}
              </Button>
            )}
            <Button variant="outline" asChild>
              <Link to="/projects/$project/$environment/$app/doctor" params={{ project, environment, app }}>
                <StethoscopeIcon aria-hidden="true" />
                {t('doctor.open')}
              </Link>
            </Button>
            <Button variant="outline" disabled={restart.isPending} onClick={() => restart.mutate()}>
              <RotateCwIcon aria-hidden="true" />
              {restart.isPending ? t('app.restarting') : t('app.restart')}
            </Button>
          </>
        }
      />
      {scheduled && (
        <p className="text-muted-foreground text-sm">
          {t('app.scheduledJob')}{' '}
          <code dir="ltr" className="font-mono">
            {scheduled.schedule}
          </code>
          {run.data && (
            <span className="text-success"> · {fill(t('app.started'), { job: run.data.job })}</span>
          )}
        </p>
      )}
      <ErrorAlert error={run.error} />
      {!a.ready && a.message && (
        <Notice tone="warning">
          <span dir="auto">{a.message}</span>
        </Notice>
      )}
      <ErrorAlert error={restart.error} />

      <Tabs value={tab} onValueChange={(value) => void openTab(value)} className="gap-6">
        <div className="-mx-1 overflow-x-auto px-1">
          <TabsList variant="line" aria-label={t('app.sections')}>
            {APP_TABS.map((value) => (
              <TabsTrigger key={value} value={value}>
                {t(TAB_LABELS[value])}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        <TabsContent value="overview" className="space-y-6">
          <div className="grid gap-6 lg:grid-cols-2">
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

          <Section title={fill(t('app.pods'), { count: data.pods.length })}>
            {data.pods.length === 0 ? (
              <p className="text-muted-foreground text-sm">{t('app.noPods')}</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('app.pod')}</TableHead>
                    <TableHead>{t('app.status')}</TableHead>
                    <TableHead>{t('app.restarts')}</TableHead>
                    <TableHead>{t('app.node')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.pods.map((pod) => (
                    <TableRow key={pod.name}>
                      <TableCell dir="ltr" className="text-start font-mono text-xs">
                        {pod.name}
                      </TableCell>
                      <TableCell>
                        <StatusBadge ready={pod.ready} label={pod.reason ?? pod.phase} />
                      </TableCell>
                      <TableCell>{pod.restarts}</TableCell>
                      <TableCell dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
                        {pod.node ?? '—'}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Section>

          <UsageCard project={project} environment={environment} app={app} />

          {a.volumes.length > 0 && <VolumesCard volumes={a.volumes} />}
        </TabsContent>

        <TabsContent value="deployments">
          <DeploymentsCard project={project} environment={environment} app={app} />
        </TabsContent>

        <TabsContent value="releases" className="space-y-6">
          <ReleasesCard project={project} environment={environment} app={app} />
          <PromoteCard project={project} environment={environment} app={app} />
        </TabsContent>

        <TabsContent value="logs">
          <LiveLogs
            project={project}
            environment={environment}
            app={app}
            processes={a.processes.filter((p) => !p.schedule).map((p) => p.name)}
          />
        </TabsContent>

        <TabsContent value="settings" className="space-y-6">
          <EnvCard
            text={formatEnvLines(a.env)}
            pending={update.isPending}
            error={update.error}
            onSave={(env) => update.mutate({ env })}
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

          <ImagePolicyCard project={project} environment={environment} app={app} />

          <DetachCard project={project} environment={environment} app={app} />

          <Section tone="danger" title={t('ui.dangerZone')}>
            <div className="flex flex-wrap items-center justify-between gap-4">
              {a.volumes.length > 0 ? (
                <div className="flex items-center gap-2">
                  <Checkbox
                    id={deleteVolumesId}
                    checked={deleteVolumes}
                    onCheckedChange={(checked) => setDeleteVolumes(checked === true)}
                  />
                  <Label htmlFor={deleteVolumesId} className="font-normal text-muted-foreground">
                    {t('app.deleteVolumes')}
                  </Label>
                </div>
              ) : (
                <span />
              )}
              <ConfirmDelete
                name={a.name}
                what={t('app.what')}
                pending={remove.isPending}
                error={remove.error}
                onConfirm={() => remove.mutate()}
              />
            </div>
          </Section>
        </TabsContent>
      </Tabs>
    </div>
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
    <Section title={t('environment.image')}>
      <form onSubmit={onSubmit} className="grid gap-4">
        <TextInput
          key={image}
          label={t('app.deployImage')}
          name="image"
          dir="ltr"
          defaultValue={image}
          required
        />
        <ErrorAlert error={error} />
        <div>
          <Button type="submit" disabled={pending}>
            {t('ui.deploy')}
          </Button>
        </div>
      </form>
    </Section>
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
    <Section
      title={
        <span className="flex items-center gap-2">
          {t('app.scale')} <Tag>{size}</Tag>
        </span>
      }
    >
      <form onSubmit={onSubmit} key={`${min}-${max}`} className="grid grid-cols-2 gap-4">
        <TextInput
          label={t('environment.replicas')}
          name="replicas"
          type="number"
          min={0}
          max={50}
          defaultValue={min}
        />
        <TextInput
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
    </Section>
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
    <Section title={t('environment.envVars')}>
      <form onSubmit={onSubmit} className="grid gap-4">
        <TextareaInput
          key={text}
          label={t('app.variables')}
          name="env"
          dir="ltr"
          defaultValue={text}
          hint={t('app.variablesHint')}
        />
        {parseError && <ErrorAlert error={new Error(parseError)} />}
        <ErrorAlert error={error} />
        <div>
          <Button type="submit" variant="secondary" disabled={pending}>
            {t('app.saveAndRollOut')}
          </Button>
        </div>
      </form>
    </Section>
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
  const columns: Column<Release>[] = [
    {
      id: 'revision',
      header: t('deployments.revision'),
      sortValue: (r) => r.revision,
      filterValue: (r) => `#${r.revision}`,
      cell: (r) => <span className="font-medium font-mono">#{r.revision}</span>,
    },
    {
      id: 'reason',
      header: t('releases.reason'),
      sortValue: (r) => r.reason,
      filterValue: (r) => `${r.reason} ${r.note ?? ''}`,
      className: 'whitespace-normal',
      cell: (r) => (
        <div className="space-y-1">
          <Tag>{r.reason}</Tag>
          {r.note && (
            <p dir="auto" className="text-muted-foreground text-xs">
              {r.note}
            </p>
          )}
        </div>
      ),
    },
    {
      id: 'image',
      header: t('environment.image'),
      filterValue: (r) => r.image,
      className: 'max-w-72',
      cell: (r) => (
        <span dir="ltr" className="block truncate font-mono text-muted-foreground text-xs">
          {r.image ?? '—'}
        </span>
      ),
    },
    {
      id: 'when',
      header: t('audit.when'),
      sortValue: (r) => r.created_at,
      filterValue: (r) => r.actor,
      className: 'text-muted-foreground text-xs',
      cell: (r) => (
        <>
          <time dateTime={new Date(r.created_at).toISOString()}>
            {new Date(r.created_at).toLocaleString(locale)}
          </time>
          {r.actor && (
            <span dir="ltr" className="block">
              {r.actor}
            </span>
          )}
        </>
      ),
    },
    {
      id: 'action',
      header: t('app.rollBack'),
      hideHeader: true,
      className: 'text-end',
      cell: (r) =>
        r.current ? (
          <Tag className="border-success/30 bg-success/10 text-success">{t('app.current')}</Tag>
        ) : (
          <Button
            variant="outline"
            size="sm"
            disabled={rollback.isPending}
            onClick={() => rollback.mutate(r.revision)}
          >
            {t('app.rollBack')}
          </Button>
        ),
    },
  ]

  return (
    <Section title={t('app.releases')}>
      <ErrorAlert error={rollback.error} />
      {releases.isError ? (
        <ErrorAlert error={releases.error} />
      ) : (
        <DataTable
          label={t('app.releases')}
          columns={columns}
          rows={releases.data}
          rowKey={(r) => String(r.revision)}
          loading={releases.isLoading}
          empty={t('app.noReleases')}
          initialSort={{ id: 'revision', desc: true }}
          pageSize={10}
        />
      )}
    </Section>
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
    <Section title={t('app.promote')} description={t('app.promoteHint')}>
      <div className="flex flex-wrap items-end gap-3">
        <SelectInput
          label={t('app.targetEnvironment')}
          value={target}
          className="w-auto min-w-56"
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
        </SelectInput>
        <Button
          variant="outline"
          disabled={!target || promote.isPending}
          onClick={() => promote.mutate(true)}
        >
          {t('app.previewChanges')}
        </Button>
        <Button disabled={!previewed || promote.isPending} onClick={() => promote.mutate(false)}>
          {t('app.promote')}
        </Button>
      </div>
      {result && (
        <div className="space-y-2 text-sm">
          {result.dry_run ? (
            <p>
              {result.changes.length
                ? fill(t('app.changesIn'), { target })
                : fill(t('app.upToDate'), { target })}
            </p>
          ) : (
            <Notice tone="success">
              {fill(t('app.promoted'), { target })}
              {result.created ? ` (${t('app.appCreated')})` : ''}.
            </Notice>
          )}
          {result.changes.length > 0 && (
            <ul
              dir="ltr"
              className="list-disc space-y-0.5 rounded-md bg-muted/50 py-2 ps-7 pe-3 text-start font-mono text-xs"
            >
              {result.changes.map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
          )}
          {result.warnings.map((w) => (
            <p key={w} dir="auto" className="text-warning text-xs">
              {w}
            </p>
          ))}
        </div>
      )}
      <ErrorAlert error={promote.error} />
    </Section>
  )
}

const dnsColor: Record<string, string> = {
  ok: 'text-success',
  mismatch: 'text-destructive',
  unresolved: 'text-warning',
  unknown: 'text-muted-foreground',
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
    <Section
      title={t('environment.domains')}
      actions={
        <Button variant="outline" size="sm" disabled={check.isPending} onClick={() => check.mutate()}>
          {check.isPending ? t('app.checking') : t('app.checkDns')}
        </Button>
      }
    >
      <form onSubmit={onSubmit} className="flex flex-wrap items-start gap-3">
        <TextInput
          key={domains.join(' ')}
          label={t('app.customDomains')}
          name="domains"
          dir="ltr"
          defaultValue={domains.join(' ')}
          placeholder="api.example.com www.example.com"
          hint={t('app.customDomainsHint')}
          className="min-w-64 flex-1"
        />
        <Button type="submit" variant="secondary" disabled={pending} className="mt-[1.625rem]">
          {t('ui.save')}
        </Button>
      </form>
      {check.data && (
        <ul className="space-y-1 text-sm">
          {check.data.map((d) => (
            <li key={d.host} className="flex flex-wrap items-center gap-2">
              <span dir="ltr" className="font-mono">
                {d.host}
              </span>
              <span className={dnsColor[d.status] ?? ''}>{tOr(`app.dns.${d.status}`, d.status)}</span>
              <span dir="auto" className="text-muted-foreground text-xs">
                {d.message}
              </span>
            </li>
          ))}
        </ul>
      )}
      <ErrorAlert error={check.error} />
    </Section>
  )
}

function VolumesCard({ volumes }: { volumes: readonly Volume[] }) {
  const { t } = usePrefs()
  return (
    <Section title={t('app.volumes')}>
      <ul className="space-y-2 text-sm">
        {volumes.map((v) => (
          <li key={v.name} className="flex flex-wrap items-center gap-2">
            <span dir="ltr" className="font-mono">
              {v.mount_path}
            </span>
            <Tag>{v.size}</Tag>
            <span className="text-muted-foreground text-xs">
              {v.name} · {t('app.volumeKept')}
            </span>
          </li>
        ))}
      </ul>
    </Section>
  )
}
